package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/secrets"
)

// quiesceShell is the interpreter a quiesce step runs under, the same one the
// pass-3 hook mechanism uses so a quiesce file is authored exactly like a
// source's hooks.yml.
const quiesceShell = "/bin/sh"

// Quiesce makes a live-source backup application-consistent. Its Pre steps
// quiesce the source before the snapshot is assembled — pg_dump into the data
// dir, stop-stack, or `sqlite3 .backup` — and its Post steps un-quiesce it
// after. It reuses the hook shape the source lifecycle uses (internal/hooks),
// so a backup step carries pre/post hooks through the same mechanism a run does.
//
// The contract that makes it safe: a failed Pre step ABORTS the backup before
// any snapshot is taken (never a torn snapshot) and surfaces the error to be
// alerted on, while the Post steps ALWAYS run once Pre succeeded — even when the
// snapshot or a leg fails — so a quiesced source is never left frozen.
//
// Unlike a source's run hooks, quiesce steps run at the subprocess boundary
// (an admin action reaching a DB socket or the container runtime, which the
// sandbox would deny), so they are trusted operator configuration.
type Quiesce struct {
	// Pre quiesces the source; Post un-quiesces it.
	Pre  []hooks.Hook
	Post []hooks.Hook
	// Driver runs a step. Empty uses the local shell driver.
	Driver executor.Driver
	// Dir is the working directory steps run in and that a step's script path is
	// resolved against.
	Dir string
	// Redactor scrubs known secret values out of a step's retained output.
	Redactor *secrets.Redactor
}

// LoadQuiesce reads a quiesce file in the hooks.yml shape: its pre_run steps
// quiesce, its post_run steps un-quiesce. A gate is meaningless for a backup and
// is refused rather than ignored. A missing file, or one with no steps, yields
// (nil, nil): the backup runs exactly as it did before, unquiesced.
func LoadQuiesce(path string) (*Quiesce, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("backup: read quiesce %s: %w", path, err)
	}
	hk, err := hooks.Parse(data)
	if err != nil {
		return nil, err
	}
	if len(hk.Gate) > 0 {
		return nil, fmt.Errorf("backup: quiesce %s declares a gate; a quiesce has only pre_run/post_run", path)
	}
	if len(hk.PreRun) == 0 && len(hk.PostRun) == 0 {
		return nil, nil
	}
	return &Quiesce{Pre: hk.PreRun, Post: hk.PostRun, Dir: filepath.Dir(path)}, nil
}

// freeze runs the Pre steps and returns a thaw func that runs the Post steps.
// On a failed Pre step it un-quiesces best-effort and returns the error with no
// thaw func: the caller must abort before taking a snapshot. The returned thaw
// is always safe to call and logs, rather than returns, a failed un-quiesce —
// the snapshot is already taken by then, and the operator needs the warning, not
// a lost backup.
func (q *Quiesce) freeze(ctx context.Context, log *slog.Logger) (thaw func(), err error) {
	driver := q.Driver
	if driver == nil {
		driver = executor.NewShell()
	}
	if err := q.runSteps(ctx, driver, q.Pre); err != nil {
		// A half-finished freeze must not leave the source quiesced: thaw on the
		// way out (best effort, out of the cancelled context), then abort.
		if terr := q.runSteps(context.WithoutCancel(ctx), driver, q.Post); terr != nil {
			log.Warn("backup quiesce: un-quiesce after a failed quiesce also failed", "error", terr)
		}
		return nil, err
	}
	return func() {
		if err := q.runSteps(context.WithoutCancel(ctx), driver, q.Post); err != nil {
			log.Warn("backup quiesce: un-quiesce failed; the source may still be quiesced", "error", err)
		}
	}, nil
}

// runSteps runs a step list in order, stopping at the first non-zero exit.
// Correctness is the exit code (AGENTS.md invariant 3); the output is kept only
// to explain a failure and is scrubbed by the redactor first.
func (q *Quiesce) runSteps(ctx context.Context, driver executor.Driver, list []hooks.Hook) error {
	for _, h := range list {
		argv := []string{quiesceShell, "-c", h.Run}
		if h.Script != "" {
			argv = []string{quiesceShell, filepath.Join(q.Dir, h.Script)}
		}
		res, err := runTool(ctx, driver, executor.Spec{
			Kind: hooks.JobKind,
			Argv: argv,
			Dir:  q.Dir,
			Env:  toolEnv(nil),
		}, q.Redactor)
		if err != nil {
			return fmt.Errorf("quiesce step %q: %w", h.Name, err)
		}
		if res.exitCode != 0 {
			return fmt.Errorf("quiesce step %q: exit %d: %s", h.Name, res.exitCode, res.tail(maxKeptLines))
		}
	}
	return nil
}
