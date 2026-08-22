package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// Target kinds. The stored kind decides which leg runs.
const (
	KindResticLocal = "restic_local"
	KindResticS3    = "restic_s3"
	KindGit         = "git"
)

// stampLayout names snapshots in the git leg; sorting the names sorts by time.
const stampLayout = "20060102T150405Z"

// maxKeptLines bounds how much subprocess output a leg holds on to for its
// error message.
const maxKeptLines = 200

// Target is one leg of the fan-out: the stored row plus the process
// environment that leg needs. S3 credentials arrive here — AWS_ACCESS_KEY_ID
// and AWS_SECRET_ACCESS_KEY, plus AWS_DEFAULT_REGION where the endpoint cares —
// so they can come from sealed secrets without this package knowing how they
// were stored.
type Target struct {
	store.BackupTarget
	Env map[string]string
}

// Options configures an Engine. The zero value is usable.
type Options struct {
	// StageRoot is the parent directory of the 0700 staging dir. Empty uses
	// the system temp dir.
	StageRoot string
	// Driver runs the subprocesses. Empty uses the local shell driver.
	Driver executor.Driver
	// Now overrides the clock, for tests.
	Now func() time.Time
	// ProbeTimeout bounds the reachability steps — opening or creating the
	// repository — so an unreachable target fails its leg instead of retrying
	// through the backup window. Zero uses defaultProbeTimeout. Transferring
	// the data is never bounded this way.
	ProbeTimeout time.Duration
	// Logger receives warnings that are not leg failures — e.g. an s3 target
	// falling back to its process environment because it has no sealed
	// credentials. Zero uses slog.Default().
	Logger *slog.Logger
	// Quiesce, when set, wraps the snapshot in application-consistency hooks:
	// its Pre steps quiesce the source before staging and its Post steps
	// un-quiesce it after. A failed Pre step aborts the Run before any snapshot
	// is taken. Nil means no quiesce — the snapshot is assembled directly, as
	// before.
	Quiesce *Quiesce
}

// defaultProbeTimeout is how long a target has to prove it is reachable.
const defaultProbeTimeout = 30 * time.Second

// Engine assembles a snapshot and sends it to targets.
type Engine struct {
	store *store.Store
	env   *secrets.Envelope
	opts  Options
}

// New returns an Engine over a store and an open envelope.
func New(st *store.Store, env *secrets.Envelope, opts Options) *Engine {
	if opts.Driver == nil {
		opts.Driver = executor.NewShell()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Engine{store: st, env: env, opts: opts}
}

func (e *Engine) probeTimeout() time.Duration {
	if e.opts.ProbeTimeout > 0 {
		return e.opts.ProbeTimeout
	}
	return defaultProbeTimeout
}

// LegResult is one target's outcome. A leg that failed carries its error here
// and in its backup_run row; it never aborts the other legs.
type LegResult struct {
	Target     string
	Kind       string
	RunID      int64
	State      string
	SnapshotID string
	BytesAdded int64
	Err        error
}

// Report is the outcome of one Run.
type Report struct {
	StartedAt time.Time
	Manifest  Manifest
	Legs      []LegResult
}

// Err joins the errors of every failed leg, or returns nil when all succeeded.
func (r Report) Err() error {
	var errs []error
	for _, l := range r.Legs {
		if l.Err != nil {
			errs = append(errs, fmt.Errorf("target %s: %w", l.Target, l.Err))
		}
	}
	return errors.Join(errs...)
}

// Run assembles the staging tree once and sends it to every target. Targets
// are independent: each gets its own backup_run row, and a failing one leaves
// the rest untouched. The returned error is non-nil only when no leg could be
// attempted at all; per-leg failures live in the Report.
func (e *Engine) Run(ctx context.Context, targets []Target) (Report, error) {
	rep := Report{StartedAt: e.opts.Now()}

	// Quiesce the source before anything is staged: a failed quiesce aborts here,
	// so the snapshot is never taken over a torn source. Once quiesced, the
	// un-quiesce is deferred and runs whatever happens to the legs below.
	if e.opts.Quiesce != nil {
		thaw, err := e.opts.Quiesce.freeze(ctx, e.opts.Logger)
		if err != nil {
			return rep, fmt.Errorf("backup: quiesce aborted before any snapshot was taken: %w", err)
		}
		defer thaw()
	}

	stage, err := os.MkdirTemp(e.opts.StageRoot, "exocomp-backup-")
	if err != nil {
		return rep, fmt.Errorf("backup: create staging dir: %w", err)
	}
	// MkdirTemp creates 0700; the snapshot and the keyring live here.
	defer func() { _ = os.RemoveAll(stage) }()

	manifest, err := e.stage(ctx, stage)
	if err != nil {
		return rep, err
	}
	rep.Manifest = manifest

	for _, t := range targets {
		rep.Legs = append(rep.Legs, e.runLeg(ctx, t, stage))
	}
	return rep, nil
}

// stage writes the snapshot, the keyring, and the manifest into dir.
func (e *Engine) stage(ctx context.Context, dir string) (Manifest, error) {
	if err := e.store.VacuumInto(ctx, filepath.Join(dir, SnapshotName)); err != nil {
		return Manifest{}, err
	}

	keyring, err := e.env.KeyringAge()
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: keyring: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, KeyringName), keyring, 0o600); err != nil {
		return Manifest{}, fmt.Errorf("backup: stage keyring: %w", err)
	}
	version := e.env.KeyringVersion()
	if err := os.WriteFile(filepath.Join(dir, KeyringVersionName),
		secrets.MarshalKeyringVersion(version), 0o600); err != nil {
		return Manifest{}, fmt.Errorf("backup: stage keyring version: %w", err)
	}

	schema, err := e.store.SchemaVersion(ctx)
	if err != nil {
		return Manifest{}, err
	}
	return WriteManifest(dir, Manifest{
		SchemaVersion:  schema,
		KeyringVersion: version.Version,
		CreatedAt:      e.opts.Now().UTC(),
		ToolVersions:   toolVersions(),
	})
}

func (e *Engine) runLeg(ctx context.Context, t Target, stage string) LegResult {
	res := LegResult{Target: t.Name, Kind: t.Kind, State: store.BackupFailed}

	run, err := e.store.StartBackupRun(ctx, t.ID)
	if err != nil {
		res.Err = err
		return res
	}
	res.RunID = run.ID

	snapshot, added, legErr := e.dispatch(ctx, t, stage)
	run.SnapshotID = snapshot
	run.BytesAdded = added
	switch {
	case legErr == nil:
		run.State = store.BackupSucceeded
	case ctx.Err() != nil:
		run.State = store.BackupCancelled
		run.Error = legErr.Error()
	default:
		run.State = store.BackupFailed
		run.Error = legErr.Error()
	}
	res.State = run.State
	res.SnapshotID = snapshot
	res.BytesAdded = added
	res.Err = legErr

	// The row this leg opened is closed even when the leg was cancelled: a
	// backup_run left running is a run nothing is running, and the write that
	// says so must not itself run under the context that was just cancelled.
	if err := e.store.FinishBackupRun(context.WithoutCancel(ctx), run); err != nil {
		res.Err = errors.Join(res.Err, err)
	}
	return res
}

func (e *Engine) dispatch(ctx context.Context, t Target, stage string) (string, int64, error) {
	// Per leg, because the subkey is per target: the tag says this manifest was
	// written for this target by something holding the DEK.
	if err := e.authenticate(t, stage); err != nil {
		return "", 0, err
	}
	switch t.Kind {
	case KindResticLocal, KindResticS3:
		return e.resticLeg(ctx, t, stage)
	case KindGit:
		return e.gitLeg(ctx, t, stage)
	default:
		return "", 0, fmt.Errorf("backup: unknown target kind %q", t.Kind)
	}
}

// authenticate writes the target's tag over the staged manifest.
func (e *Engine) authenticate(t Target, stage string) error {
	key := e.env.Derive(ManifestMACPurpose, t.Name)
	if len(key) != dekLen {
		return errors.New("backup: envelope is closed")
	}
	defer clear(key)
	return AuthenticateManifest(stage, key, t.Name)
}

// toolResult is what one subprocess left behind. ExitCode alone decides
// success (AGENTS.md invariant 3); the output only explains a failure.
type toolResult struct {
	exitCode int
	stdout   []string
	stderr   []string
}

func (r toolResult) tail(n int) string {
	lines := r.stderr
	if len(lines) == 0 {
		lines = r.stdout
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

func (e *Engine) runTool(ctx context.Context, spec executor.Spec, red *secrets.Redactor) (toolResult, error) {
	return runTool(ctx, e.opts.Driver, spec, red)
}

// runTool runs one subprocess through the executor driver. red scrubs known
// secret values out of every retained line, so a tool that echoes its
// credentials cannot put them into a backup_run error.
func runTool(ctx context.Context, driver executor.Driver, spec executor.Spec, red *secrets.Redactor) (toolResult, error) {
	sink := &lineSink{red: red}
	result, err := driver.Run(ctx, spec, sink)
	if err != nil {
		return toolResult{}, fmt.Errorf("backup: run %s: %w", spec.Argv[0], err)
	}
	return toolResult{exitCode: result.ExitCode, stdout: sink.stdout, stderr: sink.stderr}, nil
}

type lineSink struct {
	mu     sync.Mutex
	red    *secrets.Redactor
	stdout []string
	stderr []string
}

func (s *lineSink) Emit(ev executor.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	line := ev.Msg
	if s.red != nil {
		line = string(s.red.Redact([]byte(line)))
	}
	if ev.Level == executor.LevelError {
		s.stderr = appendCapped(s.stderr, line)
		return
	}
	s.stdout = appendCapped(s.stdout, line)
}

func appendCapped(lines []string, line string) []string {
	if len(lines) >= maxKeptLines {
		lines = lines[1:]
	}
	return append(lines, line)
}

// toolEnv builds a subprocess environment explicitly instead of inheriting the
// caller's: a leg must get its storage credentials from its own target
// config, never by accident from whatever the operator exported.
func toolEnv(extra map[string]string) []string {
	env := map[string]string{}
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	maps.Copy(env, extra)

	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}
