package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// The upgrade choreography's steps, in order. Each is one ledger step and one
// gated queue job; the sequencer runs step N+1 only once step N succeeded
// (ADR 0008). The swap is deliberately LAST among the mutating steps: a crash
// before it leaves the old binary live, a crash after it is recoverable by the
// rollback path, and nothing in between can leave a half-written binary.
const (
	kindSnapshot = "upgrade.snapshot"
	kindStage    = "upgrade.stage"
	kindVerify   = "upgrade.verify"
	kindSwap     = "upgrade.swap"
	kindHealth   = "upgrade.health"
)

// The step numbers matching the ordered list, so a driver step can address its
// own ledger row (rollback markers) without threading the index through the
// queue spec.
const (
	stepSnapshot = 1
	stepStage    = 2
	stepVerify   = 3
	stepSwap     = 4
	stepHealth   = 5
)

// upgradeSubdir is the directory under the data dir that holds one dir per
// upgrade (the snapshot rollback point and the preserved prior binary). Its
// entries are named by the workflow id, whose timestamp prefix sorts
// chronologically — so the newest is the lexical maximum, which is how
// `rollback` finds the most recent pre-upgrade point.
const (
	upgradeSubdir  = "upgrade"
	upgradeIDPre   = "upgrade-"
	stagedBinPre   = ".exocomp-upgrade."
	snapshotSubdir = "snapshot"
	priorBinName   = "prior-binary"
)

func init() {
	registerCommand(newUpgradeCmd)
	// The upgrade steps run inline over upgradeDriver, which the daemon's queue
	// driver does not register. A hard crash mid-upgrade must not have the boot
	// ResumeAll re-drive them through a driver that cannot execute the kind; mark
	// them so the daemon leaves an interrupted upgrade for `rollback` to settle.
	queue.RegisterInlineOnlyKind(kindSnapshot, kindStage, kindVerify, kindSwap, kindHealth)
}

func newUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade <path-to-new-binary>",
		Short: "Swap in a new exocomp binary as a resumable, rollback-guarded workflow",
		Long: "Runs a step-ledger choreography (ADR 0008): snapshot the data dir, stage the " +
			"new binary beside the current one, verify it (version + doctor), atomically swap " +
			"it in, then health-check the swapped binary. Any failed step rolls back — the prior " +
			"binary and the pre-upgrade data snapshot are restored, leaving the system as it was.\n\n" +
			"exocomp never escalates privilege: it operates only within the data dir and the " +
			"binary's own directory. The swap takes effect on the next process start; restart " +
			"the service (e.g. systemctl --user restart exocomp) to pick it up.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpgrade(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
}

// runUpgrade drives the whole choreography inline (foreground): it starts a
// queue over an upgrade driver, hands the sequencer the ordered step list, and
// runs it to a terminal end. On failure it rolls back and returns a non-zero
// error; on success it reports the new version and the systemd restart handoff.
func runUpgrade(ctx context.Context, out io.Writer, source string) error {
	live, err := resolveSelf()
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	return upgradeWithBinary(ctx, out, live, source)
}

// upgradeWithBinary is the choreography with the live binary already resolved,
// so a test can drive it against a stand-in binary rather than the test process.
func upgradeWithBinary(ctx context.Context, out io.Writer, live, source string) error {
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("upgrade: resolve data dir: %w", err)
	}

	// A running daemon holds the binary as its live process image and drains the
	// same job table this workflow enqueues into: swapping under it, or racing it
	// for the gated steps, is not safe. Stopping it is the operator's job.
	if daemonRunning(ctx, dir) {
		return errors.New("upgrade: a daemon is serving this data dir; stop it first " +
			"(the service restart after the swap is the handoff), then re-run upgrade")
	}

	src, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("upgrade: resolve source path: %w", err)
	}
	if info, err := os.Stat(src); err != nil {
		return fmt.Errorf("upgrade: new binary %s: %w", src, err)
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("upgrade: new binary %s is not a regular file", src)
	}

	binDir := filepath.Dir(live)
	if err := unix.Access(binDir, unix.W_OK|unix.X_OK); err != nil {
		// The one place privilege would be needed — and the one place exocomp
		// refuses to reach for it. Tell the operator exactly what to run.
		return fmt.Errorf("upgrade: the binary's directory %s is not writable by this user (%w); "+
			"exocomp never escalates privilege — install the new binary yourself:\n"+
			"  install -m0755 %s %s\n"+
			"then restart the service", binDir, err, src, live)
	}

	workflowID := upgradeIDPre + time.Now().UTC().Format("20060102T150405.000000000Z")
	paths := upgradePathsFor(dir, live, src, workflowID)

	st, err := openJobStore(ctx, out)
	if err != nil {
		return err
	}
	closed := false
	closeStore := func() {
		if !closed {
			_ = st.Close()
			closed = true
		}
	}
	defer closeStore()

	drv := &upgradeDriver{st: st, workflowID: workflowID, paths: paths}
	q := queue.New(st, drv, queue.Options{
		Workers:      1,
		PollInterval: 20 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	seq := queue.NewSequencer(q)

	steps := []queue.Step{
		upgradeStep(kindSnapshot, workflowID, "snapshot"),
		upgradeStep(kindStage, workflowID, "stage"),
		upgradeStep(kindVerify, workflowID, "verify"),
		upgradeStep(kindSwap, workflowID, "swap"),
		upgradeStep(kindHealth, workflowID, "health"),
	}

	qctx, qcancel := context.WithCancel(ctx)
	qdone := make(chan error, 1)
	go func() { qdone <- q.Run(qctx) }()

	_, _ = fmt.Fprintf(out, "upgrade %s: %d-step ledger workflow\n", workflowID, len(steps))
	runErr := seq.Start(ctx, workflowID, steps)
	if runErr == nil {
		runErr = seq.Run(ctx, workflowID)
	}
	qcancel()
	<-qdone
	if runErr != nil {
		return fmt.Errorf("upgrade: drive workflow: %w", runErr)
	}

	ledger, err := st.GetWorkflow(ctx, workflowID)
	if err != nil {
		return err
	}
	printLedger(out, ledger)

	if store.WorkflowStatus(ledger) == store.WorkflowSucceeded {
		return finishUpgradeSuccess(ctx, out, paths)
	}

	// A failed step: roll back to the pre-upgrade binary and data snapshot. The
	// store must be closed before the database file is replaced under it.
	failed := firstFailedStep(ledger)
	closeStore()
	_ = os.Remove(paths.stagedPath) // a staged binary that never swapped in
	if err := restorePriorState(ctx, paths, workflowID, out); err != nil {
		return fmt.Errorf("upgrade: step %d (%s) failed AND rollback hit an error: %w — "+
			"inspect %s by hand", failed.StepNo, failed.Kind, err, paths.workflowDir)
	}
	return fmt.Errorf("upgrade: step %d (%s) failed: %s; rolled back to the previous binary and "+
		"data snapshot, the system is as it was", failed.StepNo, failed.Kind, stepDetail(failed))
}

// finishUpgradeSuccess reports the swapped-in version and the restart handoff,
// then prunes stale upgrade points. The snapshot and prior binary of THIS
// upgrade are kept: they are what a later `rollback` returns to.
func finishUpgradeSuccess(ctx context.Context, out io.Writer, paths upgradePaths) error {
	ver, err := runExoBinary(ctx, paths.livePath, paths.xdgDataHome, "version")
	if err != nil {
		return fmt.Errorf("upgrade: swapped in, but the new binary would not report its version: %w", err)
	}
	_, _ = fmt.Fprintf(out, "\nupgrade complete: now running %s\n", strings.TrimSpace(ver))
	_, _ = fmt.Fprintf(out, "rollback point kept at %s (exocomp rollback restores it)\n", paths.workflowDir)
	_, _ = fmt.Fprintf(out, "the swap takes effect on the next start; restart the service to pick it up "+
		"(e.g. systemctl --user restart exocomp). exocomp does not restart itself in v1.\n")
	pruneOldUpgrades(filepath.Join(paths.dataDir, upgradeSubdir), paths.workflowDir, out)
	return nil
}

// upgradeStep builds one workflow step. The kind is what the driver dispatches
// on; the argv is never exec'd (the upgrade driver intercepts every kind) but a
// job row needs a legible, non-empty argv, same as a backup job's.
func upgradeStep(kind, workflowID, name string) queue.Step {
	return queue.Step{
		Kind: kind,
		Spec: queue.Spec{
			Runbook:        "upgrade",
			Kind:           kind,
			Trigger:        "manual",
			Argv:           []string{"exocomp-upgrade", workflowID, name},
			ConcurrencyKey: "upgrade",
		},
	}
}

// upgradePaths is every path one upgrade touches, all derivable from the data
// dir, the live binary, and the workflow id — so `rollback` can reconstruct
// them from the workflow id alone, with no side state to keep consistent.
type upgradePaths struct {
	dataDir     string
	xdgDataHome string
	livePath    string
	binDir      string
	stagedPath  string
	source      string
	workflowDir string
	snapshotDir string
	priorPath   string
}

func upgradePathsFor(dataDir, live, source, workflowID string) upgradePaths {
	binDir := filepath.Dir(live)
	wfDir := filepath.Join(dataDir, upgradeSubdir, workflowID)
	return upgradePaths{
		dataDir: dataDir,
		// A spawned exocomp resolves its data dir as XDG_DATA_HOME/exocomp, so the
		// value handed to it is the PARENT of this data dir, not the data dir
		// itself — otherwise the child would look in a nested exocomp/exocomp.
		xdgDataHome: filepath.Dir(dataDir),
		livePath:    live,
		binDir:      binDir,
		stagedPath:  filepath.Join(binDir, stagedBinPre+workflowID),
		source:      source,
		workflowDir: wfDir,
		snapshotDir: filepath.Join(wfDir, snapshotSubdir),
		priorPath:   filepath.Join(wfDir, priorBinName),
	}
}

// upgradeDriver runs one upgrade step per queue job, dispatching on the job's
// kind. It implements executor.Driver so the steps run through the same queue
// as every other execution path: an upgrade step is Go code doing local file
// and subprocess work, not one argv a shell driver could exec, so — like the
// backup runner — it needs its own driver rather than an argv. Correctness is
// exit-code-only (AGENTS.md invariant 3): a step's error becomes exit 1, which
// the sequencer records as a failed ledger step; nil becomes exit 0.
type upgradeDriver struct {
	st         *store.Store
	workflowID string
	paths      upgradePaths
}

// Run implements executor.Driver.
func (d *upgradeDriver) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	var err error
	switch spec.Kind {
	case kindSnapshot:
		err = d.snapshot(ctx, sink)
	case kindStage:
		err = d.stage(sink)
	case kindVerify:
		err = d.verify(ctx, sink)
	case kindSwap:
		err = d.swap(ctx, sink)
	case kindHealth:
		err = d.health(ctx, sink)
	default:
		return executor.Result{}, fmt.Errorf("upgrade: unknown step kind %q", spec.Kind)
	}
	if err != nil {
		sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: executor.LevelError, Msg: err.Error()})
		return executor.Result{ExitCode: 1}, nil
	}
	return executor.Result{ExitCode: 0}, nil
}

// snapshot takes the restorable point: a vacuumed copy of the database plus its
// manifest, in the workflow's own dir. The path is recorded as the step's
// rollback marker so the ledger itself says where the undo point is.
func (d *upgradeDriver) snapshot(ctx context.Context, sink executor.EventSink) error {
	m, err := backup.LocalSnapshot(ctx, d.st, d.paths.snapshotDir)
	if err != nil {
		return err
	}
	if err := d.st.SetRollbackMarker(ctx, d.workflowID, stepSnapshot, d.paths.snapshotDir); err != nil {
		return err
	}
	sink.Emit(infoEvent(fmt.Sprintf("snapshot: %d file(s), schema %d, at %s",
		len(m.Files), m.SchemaVersion, d.paths.snapshotDir)))
	return nil
}

// stage copies the new binary beside the live one, without touching the live
// one: staged on the same filesystem so the later swap is an atomic rename.
func (d *upgradeDriver) stage(sink executor.EventSink) error {
	if err := copyFileMode(d.paths.source, d.paths.stagedPath, 0o700); err != nil {
		return fmt.Errorf("upgrade: stage new binary: %w", err)
	}
	sink.Emit(infoEvent("stage: new binary placed beside the live one at " + d.paths.stagedPath))
	return nil
}

// verify runs the staged binary's `version` and `doctor` against a throwaway
// data dir, so a check never mutates the live state, and requires both to
// succeed with a non-empty version. A binary that cannot report a version or
// pass its own doctor is not one to swap in.
func (d *upgradeDriver) verify(ctx context.Context, sink executor.EventSink) error {
	scratch, err := os.MkdirTemp("", "exocomp-verify-")
	if err != nil {
		return fmt.Errorf("upgrade: verify scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	ver, err := runExoBinary(ctx, d.paths.stagedPath, scratch, "version")
	if err != nil {
		return fmt.Errorf("upgrade: staged binary failed `version`: %w", err)
	}
	if strings.TrimSpace(ver) == "" {
		return errors.New("upgrade: staged binary reported an empty version")
	}
	if _, err := runExoBinary(ctx, d.paths.stagedPath, scratch, "doctor"); err != nil {
		return fmt.Errorf("upgrade: staged binary failed `doctor`: %w", err)
	}
	sink.Emit(infoEvent(fmt.Sprintf("verify: staged binary reports %q and doctor is green", strings.TrimSpace(ver))))
	return nil
}

// swap preserves the live binary, then atomically renames the staged one into
// its place — the last mutating step. The rename replaces the directory entry
// while the running process keeps the old inode, so overwriting a live
// executable is safe. The preserved prior is recorded as the step's rollback
// marker.
func (d *upgradeDriver) swap(ctx context.Context, sink executor.EventSink) error {
	if err := copyFileMode(d.paths.livePath, d.paths.priorPath, 0o700); err != nil {
		return fmt.Errorf("upgrade: preserve prior binary: %w", err)
	}
	if err := d.st.SetRollbackMarker(ctx, d.workflowID, stepSwap, d.paths.priorPath); err != nil {
		return err
	}
	if err := os.Rename(d.paths.stagedPath, d.paths.livePath); err != nil {
		return fmt.Errorf("upgrade: swap binary into place: %w", err)
	}
	sink.Emit(infoEvent(fmt.Sprintf("swap: %s is now the live binary; prior preserved at %s",
		d.paths.livePath, d.paths.priorPath)))
	return nil
}

// health runs the just-swapped binary's `version` and `doctor` against the live
// data dir — the real post-swap check that the new binary works with real
// state. A failure here rolls the whole thing back.
func (d *upgradeDriver) health(ctx context.Context, sink executor.EventSink) error {
	ver, err := runExoBinary(ctx, d.paths.livePath, d.paths.xdgDataHome, "version")
	if err != nil {
		return fmt.Errorf("upgrade: swapped binary failed post-swap `version`: %w", err)
	}
	if _, err := runExoBinary(ctx, d.paths.livePath, d.paths.xdgDataHome, "doctor"); err != nil {
		return fmt.Errorf("upgrade: swapped binary failed post-swap `doctor`: %w", err)
	}
	sink.Emit(infoEvent(fmt.Sprintf("health: swapped binary reports %q, doctor is green", strings.TrimSpace(ver))))
	return nil
}

// restorePriorState is the rollback effect, shared by the automatic rollback on
// a failed upgrade and the manual `rollback` command: put back the prior binary
// if the swap happened, and restore the data snapshot. It never runs sudo and
// touches only the data dir and the binary's own directory.
func restorePriorState(ctx context.Context, paths upgradePaths, workflowID string, out io.Writer) error {
	var errs []error

	// The binary is restored only if the swap actually ran: the rename consumes
	// the staged file, so a staged file still present means the live binary was
	// never touched and there is nothing to undo.
	if fileExists(paths.priorPath) && !fileExists(paths.stagedPath) {
		if err := replaceBinary(paths.priorPath, paths.livePath); err != nil {
			errs = append(errs, err)
		} else {
			_, _ = fmt.Fprintf(out, "rollback: restored prior binary -> %s\n", paths.livePath)
		}
	}

	if fileExists(filepath.Join(paths.snapshotDir, backup.ManifestName)) {
		if err := backup.LocalRestore(paths.snapshotDir, paths.dataDir); err != nil {
			errs = append(errs, err)
		} else {
			// The snapshot was taken mid-workflow, so it carries this upgrade's
			// own ledger rows in an in-flight state. Drive them terminal in the
			// restored database, or a later boot sweep would see an incomplete
			// workflow and try to resume it through a driver that does not know
			// upgrade steps.
			if err := terminalizeWorkflow(ctx, paths.dataDir, workflowID); err != nil {
				errs = append(errs, err)
			}
			_, _ = fmt.Fprintf(out, "rollback: restored data snapshot from %s\n", paths.snapshotDir)
		}
	}
	return errors.Join(errs...)
}

// terminalizeWorkflow drives every non-terminal step of a workflow to a
// terminal state, so a restored snapshot's in-flight ledger rows leave nothing
// a resume would pick up. A snapshot that predated the workflow has no rows to
// retire, which is not an error.
func terminalizeWorkflow(ctx context.Context, dataDir, workflowID string) error {
	st, err := openStoreAt(dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	steps, err := st.GetWorkflow(ctx, workflowID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	for _, s := range steps {
		if s.Status == store.StepRunning {
			if err := st.CompleteStep(ctx, workflowID, s.StepNo, store.StepFailed,
				`{"note":"terminated by rollback"}`); err != nil {
				return err
			}
		}
	}
	_, err = st.BlockPendingSteps(ctx, workflowID)
	return err
}

// latestUpgradeDir returns the workflow id and dir of the most recent upgrade
// that actually took effect — one that preserved a prior binary when it swapped.
// Workflow ids sort chronologically, so the newest is the lexical maximum. A
// failed upgrade that already self-reverted leaves no prior binary and is
// skipped: there is nothing for a manual rollback to undo there. The one it
// finds also holds the data snapshot, since the swap step runs after it.
func latestUpgradeDir(upgradeRoot string) (string, string, error) {
	entries, err := os.ReadDir(upgradeRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", errNoUpgradePoint
		}
		return "", "", fmt.Errorf("rollback: read %s: %w", upgradeRoot, err)
	}
	best := ""
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), upgradeIDPre) {
			continue
		}
		prior := filepath.Join(upgradeRoot, e.Name(), priorBinName)
		manifest := filepath.Join(upgradeRoot, e.Name(), snapshotSubdir, backup.ManifestName)
		if !fileExists(prior) || !fileExists(manifest) {
			continue
		}
		if e.Name() > best {
			best = e.Name()
		}
	}
	if best == "" {
		return "", "", errNoUpgradePoint
	}
	return best, filepath.Join(upgradeRoot, best), nil
}

var errNoUpgradePoint = errors.New("no upgrade rollback point found")

// pruneOldUpgrades keeps the most recent upgradeKeep rollback points and
// removes the rest — each holds a preserved binary, so they are not free to
// keep forever. keep is the just-succeeded upgrade, always retained.
func pruneOldUpgrades(upgradeRoot, keep string, out io.Writer) {
	entries, err := os.ReadDir(upgradeRoot)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), upgradeIDPre) {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) <= upgradeKeep {
		return
	}
	// dirs sort chronologically by name; drop the oldest beyond the keep count.
	// os.ReadDir already returns entries sorted by name.
	keepBase := filepath.Base(keep)
	for _, name := range dirs[:len(dirs)-upgradeKeep] {
		if name == keepBase {
			continue
		}
		if err := os.RemoveAll(filepath.Join(upgradeRoot, name)); err == nil {
			_, _ = fmt.Fprintf(out, "pruned old rollback point %s\n", name)
		}
	}
}

// upgradeKeep is how many upgrade rollback points to retain.
const upgradeKeep = 3

// --- small local helpers ---------------------------------------------------

func infoEvent(msg string) executor.Event {
	return executor.Event{Ts: time.Now().UTC(), Level: executor.LevelInfo, Msg: msg}
}

func firstFailedStep(steps []store.WorkflowStep) store.WorkflowStep {
	for _, s := range steps {
		if s.Status == store.StepFailed {
			return s
		}
	}
	return store.WorkflowStep{}
}

func stepDetail(s store.WorkflowStep) string {
	if s.ResultJSON == "" {
		return "(no detail)"
	}
	return s.ResultJSON
}

func printLedger(w io.Writer, steps []store.WorkflowStep) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STEP\tKIND\tSTATUS")
	for _, s := range steps {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\n", s.StepNo, s.Kind, s.Status)
	}
	_ = tw.Flush()
}

// resolveSelf returns the absolute, symlink-resolved path of the running
// binary — the file an upgrade replaces and a rollback restores.
func resolveSelf() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runExoBinary runs an exocomp binary with an explicit data dir and returns its
// stdout. Any non-zero exit or spawn failure is an error — exit-code-only, the
// same doctrine every driver here follows.
func runExoBinary(ctx context.Context, bin, dataHome string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = envWithDataHome(dataHome)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(errb.String())
		if detail == "" {
			detail = strings.TrimSpace(out.String())
		}
		if detail != "" {
			return out.String(), fmt.Errorf("%w: %s", err, firstLine([]byte(detail)))
		}
		return out.String(), err
	}
	return out.String(), nil
}

// envWithDataHome returns the process environment with XDG_DATA_HOME forced to
// home, so a spawned exocomp reads and writes the data dir under it rather than
// the inherited one.
func envWithDataHome(home string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "XDG_DATA_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "XDG_DATA_HOME="+home)
}

// copyFileMode copies src to dst (truncating dst) with the given mode.
func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// replaceBinary installs src over dst atomically: it writes a copy beside dst
// and renames it into place, so it works whether or not dst is the running
// process and whether or not src is on the same filesystem as dst.
func replaceBinary(src, dst string) error {
	tmp := dst + ".rollback"
	if err := copyFileMode(src, tmp, 0o700); err != nil {
		return fmt.Errorf("upgrade: stage rollback binary: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("upgrade: install rollback binary: %w", err)
	}
	return nil
}
