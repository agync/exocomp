package backup

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// fullID returns a 64-hex restic-shaped snapshot id built on a marker.
func fullID(marker string) string {
	return marker + strings.Repeat("0", 64-len(marker))
}

// seedRun writes a succeeded backup_run row for a target, the durable record a
// forget job's guard reads. The second-seeded run sorts newest (started_at
// DESC, id DESC), so "older" then "newest" ids order the history.
func seedRun(t *testing.T, st *store.Store, targetID int64, snapshot string, bytes int64) {
	t.Helper()
	run, err := st.StartBackupRun(t.Context(), targetID)
	if err != nil {
		t.Fatalf("start backup run: %v", err)
	}
	run.State = store.BackupSucceeded
	run.SnapshotID = snapshot
	run.BytesAdded = bytes
	if err := st.FinishBackupRun(t.Context(), run); err != nil {
		t.Fatalf("finish backup run: %v", err)
	}
}

// passSnapshot is what a forget subprocess env looked like at Run time: the
// fake driver records it while the runner still holds the password file open.
type passSnapshot struct {
	path string
	body string
	mode os.FileMode
}

// specDriver records every subprocess spec a forget leg asks to run, so a
// test can assert the restic argv and environment without a real repository.
type specDriver struct {
	specs  []executor.Spec
	creds  []passSnapshot
	result executor.Result
}

func (d *specDriver) Run(_ context.Context, spec executor.Spec, _ executor.EventSink) (executor.Result, error) {
	d.specs = append(d.specs, spec)
	d.creds = append(d.creds, inspectPasswordFile(spec.Env))
	return d.result, nil
}

func inspectPasswordFile(env []string) passSnapshot {
	var c passSnapshot
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "RESTIC_PASSWORD_FILE" {
			c.path = v
			if body, err := os.ReadFile(v); err == nil {
				c.body = strings.TrimSpace(string(body))
			}
			if info, err := os.Stat(v); err == nil {
				c.mode = info.Mode().Perm()
			}
		}
	}
	return c
}

func TestForgetArgvTargetOfRoundTrip(t *testing.T) {
	t.Parallel()
	target, snapshot, err := ForgetTargetOf(ForgetArgv("nas", fullID("a")))
	if err != nil || target != "nas" || snapshot != fullID("a") {
		t.Fatalf("ForgetTargetOf(ForgetArgv(%q, %q)) = %q, %q, %v", "nas", fullID("a"), target, snapshot, err)
	}
}

func TestForgetTargetOfRejectsBadArgv(t *testing.T) {
	t.Parallel()
	bad := [][]string{
		nil,
		{"exocomp-forget"},
		{"exocomp-backup", "nas", fullID("a")}, // a real backup argv shape, not a forget one
		{"exocomp-forget", "nas"},
		{"exocomp-forget", "nas", fullID("a"), "extra"},
	}
	for _, argv := range bad {
		if _, _, err := ForgetTargetOf(argv); err == nil {
			t.Errorf("ForgetTargetOf(%v): want error", argv)
		}
	}
}

// TestRunnerForgetRefusesNewestSnapshot: a forget naming the newest succeeded
// snapshot is refused at the runner — exit 1, an error-level event, and restic
// never invoked — the defense-in-depth mirror of the web guard (SPEC §7 A12):
// the newest snapshot is the recovery point.
func TestRunnerForgetRefusesNewestSnapshot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "repo"))
	seedRun(t, f.store, target.ID, fullID("b"), 1)
	seedRun(t, f.store, target.ID, fullID("c"), 2)

	seen := &specDriver{result: executor.Result{ExitCode: 0}}
	runner := &Runner{store: f.store, env: f.env, driver: seen, targetEnv: nil}
	sink, events := collectSink()
	res, err := runner.Run(t.Context(),
		executor.Spec{Kind: ForgetJobKind, Argv: ForgetArgv(target.Name, fullID("c"))}, sink)
	if err != nil {
		t.Fatalf("Run: %v (events: %+v)", err, events())
	}
	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1 for a refused forget (events: %+v)", res.ExitCode, events())
	}
	if len(seen.specs) != 0 {
		t.Errorf("restic was invoked for the refused newest snapshot: %+v", seen.specs)
	}
	var sawRefusal bool
	for _, e := range events() {
		if e.Level == executor.LevelError && strings.Contains(e.Msg, "refused") {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Errorf("no error-level refusal event: %+v", events())
	}
}

// TestRunnerForgetRefusesUnknownSnapshot: an id that is no succeeded run of
// the target is refused the same way the web page refuses one — a stale or
// crafted id never reaches restic.
func TestRunnerForgetRefusesUnknownSnapshot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "repo"))
	seedRun(t, f.store, target.ID, fullID("a"), 1)

	seen := &specDriver{result: executor.Result{ExitCode: 0}}
	runner := &Runner{store: f.store, env: f.env, driver: seen, targetEnv: nil}
	sink, events := collectSink()
	res, err := runner.Run(t.Context(),
		executor.Spec{Kind: ForgetJobKind, Argv: ForgetArgv(target.Name, fullID("deadbeef"))}, sink)
	if err != nil {
		t.Fatalf("Run: %v (events: %+v)", err, events())
	}
	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1 for a refused forget (events: %+v)", res.ExitCode, events())
	}
	if len(seen.specs) != 0 {
		t.Errorf("restic was invoked for a snapshot not on history: %+v", seen.specs)
	}
}

// TestRunnerForgetInvokesRestic pins the subprocess shape a forget job
// produces: `restic -r <repo> forget <id> --prune` with the password carried
// by a 0600 file, never in the child's environment (invariant 4) — and the
// plain per-snapshot forget, no retention flags (the verb names one snapshot;
// the target's retention policy is re-applied on the next backup's prune).
func TestRunnerForgetInvokesRestic(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "repo"))
	older := fullID("a")
	seedRun(t, f.store, target.ID, older, 1)
	seedRun(t, f.store, target.ID, fullID("b"), 2)

	seen := &specDriver{result: executor.Result{ExitCode: 0}}
	runner := &Runner{store: f.store, env: f.env, driver: seen, targetEnv: nil}
	sink, events := collectSink()
	res, err := runner.Run(t.Context(),
		executor.Spec{Kind: ForgetJobKind, Argv: ForgetArgv(target.Name, older)}, sink)
	if err != nil {
		t.Fatalf("Run: %v (events: %+v)", err, events())
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (events: %+v)", res.ExitCode, events())
	}
	if len(seen.specs) != 1 {
		t.Fatalf("restic specs = %d, want 1: %+v", len(seen.specs), seen.specs)
	}
	spec := seen.specs[0]
	repo := localRepoPath(target.URI)
	if strings.Join(spec.Argv, " ") != "restic -r "+repo+" forget "+older+" --prune" {
		t.Errorf("argv = %v, want restic -r %s forget %s --prune", spec.Argv, repo, older)
	}

	key := f.env.Derive("restic-repo", target.Name)
	want := base64.StdEncoding.EncodeToString(key)
	cred := seen.creds[0]
	if cred.path == "" {
		t.Errorf("no RESTIC_PASSWORD_FILE in the restic env: %v", spec.Env)
	} else {
		if cred.body != want {
			t.Errorf("password file holds %q, want the derived password", cred.body)
		}
		if cred.mode != 0o600 {
			t.Errorf("password file mode = %v, want 0600", cred.mode)
		}
	}
	for _, kv := range spec.Env {
		if strings.Contains(kv, want) {
			t.Errorf("the restic password leaked into the child environment: %q", kv)
		}
	}

	var sawOutcome bool
	for _, e := range events() {
		if e.Level == executor.LevelInfo && strings.Contains(e.Msg, "forgot snapshot="+older) {
			sawOutcome = true
		}
	}
	if !sawOutcome {
		t.Errorf("no info-level outcome event: %+v", events())
	}
}

// TestRunnerForgetSettlesFailedOnResticError: a non-zero restic exit settles
// the job failed (exit 1, error-level event) — exit-code truth, the same as
// every other leg (AGENTS.md invariant 3).
func TestRunnerForgetSettlesFailedOnResticError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "repo"))
	older := fullID("a")
	seedRun(t, f.store, target.ID, older, 1)
	seedRun(t, f.store, target.ID, fullID("b"), 2)

	seen := &specDriver{result: executor.Result{ExitCode: 2}}
	runner := &Runner{store: f.store, env: f.env, driver: seen, targetEnv: nil}
	sink, events := collectSink()
	res, err := runner.Run(t.Context(),
		executor.Spec{Kind: ForgetJobKind, Argv: ForgetArgv(target.Name, older)}, sink)
	if err != nil {
		t.Fatalf("Run: %v (events: %+v)", err, events())
	}
	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1 (events: %+v)", res.ExitCode, events())
	}
	var sawError bool
	for _, e := range events() {
		if e.Level == executor.LevelError && strings.Contains(e.Msg, "forget snapshot="+older+" failed") {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("no error-level failure event: %+v", events())
	}
}

func TestRunnerForgetRejectsUnknownTarget(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	runner := &Runner{store: st, env: nil, driver: &specDriver{}, targetEnv: nil}
	sink, _ := collectSink()

	_, err := runner.Run(t.Context(),
		executor.Spec{Kind: ForgetJobKind, Argv: ForgetArgv("nope", fullID("a"))}, sink)
	if err == nil {
		t.Fatal("forget with an unregistered target: want error")
	}
}

func TestRunnerForgetRejectsDisabledTarget(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	if _, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "off", Kind: KindResticLocal, URI: t.TempDir(), Enabled: false,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	runner := &Runner{store: st, env: nil, driver: &specDriver{}, targetEnv: nil}
	sink, _ := collectSink()

	_, err := runner.Run(t.Context(),
		executor.Spec{Kind: ForgetJobKind, Argv: ForgetArgv("off", fullID("a"))}, sink)
	if err == nil {
		t.Fatal("forget against a disabled target: want error")
	}
}

func TestRunnerForgetRejectsMalformedArgv(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	runner := &Runner{store: st, env: nil, driver: &specDriver{}, targetEnv: nil}
	sink, _ := collectSink()

	_, err := runner.Run(t.Context(), executor.Spec{Kind: ForgetJobKind, Argv: []string{"whatever"}}, sink)
	if err == nil {
		t.Fatal("forget with a malformed argv: want error")
	}
}

// TestRunnerForgetRefusesDrilledSnapshot proves the A12 recovery-point guard
// holds after a real restore drill verified the newest snapshot: the runner
// still refuses to forget it, exit 1, restic never invoked.
func TestRunnerForgetRefusesDrilledSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real restic")
	}
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "repo"))

	backupRunner := NewRunner(f.store, f.env, Options{}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, _ := collectSink()
	if _, err := backupRunner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: Argv(target.Name)}, sink); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	drillRunner := NewRunner(f.store, f.env, Options{}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, _ = collectSink()
	res, err := drillRunner.Run(t.Context(), executor.Spec{Kind: DrillJobKind, Argv: DrillArgv(target.Name)}, sink)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("seed drill: %v exit=%d", err, res.ExitCode)
	}

	runs, err := f.store.ListBackupRuns(t.Context(), target.ID, 1)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].SnapshotID == "" {
		t.Fatalf("runs = %+v", runs)
	}
	newest := runs[0].SnapshotID

	seen := &specDriver{result: executor.Result{ExitCode: 0}}
	runner := &Runner{store: f.store, env: f.env, driver: seen, targetEnv: nil}
	sink, events := collectSink()
	res, err = runner.Run(t.Context(),
		executor.Spec{Kind: ForgetJobKind, Argv: ForgetArgv(target.Name, newest)}, sink)
	if err != nil {
		t.Fatalf("Run: %v (events: %+v)", err, events())
	}
	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1 for the drilled newest (events: %+v)", res.ExitCode, events())
	}
	if len(seen.specs) != 0 {
		t.Errorf("restic was invoked for the drilled newest snapshot: %+v", seen.specs)
	}
}

// TestRunnerForgetsRealSnapshot runs a real forget end to end: two backups
// through the real runner, forget the older snapshot, and prove the repository
// holds only the newest one afterwards.
func TestRunnerForgetsRealSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real restic")
	}
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "repo"))

	backupRunner := NewRunner(f.store, f.env, Options{}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, _ := collectSink()
	for range 2 {
		if _, err := backupRunner.Run(t.Context(), executor.Spec{Kind: JobKind, Argv: Argv(target.Name)}, sink); err != nil {
			t.Fatalf("seed backup: %v", err)
		}
	}

	runs, err := f.store.ListBackupRuns(t.Context(), target.ID, 2)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	newest := runs[0].SnapshotID
	older := runs[1].SnapshotID
	if older == "" || newest == "" {
		t.Fatalf("runs = %+v", runs)
	}

	forgetRunner := NewRunner(f.store, f.env, Options{}, func(store.BackupTarget) map[string]string {
		return map[string]string{"RESTIC_CACHE_DIR": f.cache}
	})
	sink, events := collectSink()
	res, err := forgetRunner.Run(t.Context(),
		executor.Spec{Kind: ForgetJobKind, Argv: ForgetArgv(target.Name, older)}, sink)
	if err != nil {
		t.Fatalf("Run: %v (events: %+v)", err, events())
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (events: %+v)", res.ExitCode, events())
	}

	key := f.env.Derive("restic-repo", target.Name)
	passFile, cleanup, err := writePasswordFile(f.cache, base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("password file: %v", err)
	}
	defer cleanup()
	repo := localRepoPath(target.URI)
	snap, err := runTool(t.Context(), executor.NewShell(), executor.Spec{
		Kind: "shell",
		Argv: []string{"restic", "-r", repo, "snapshots", "--json"},
		Env:  resticEnv(map[string]string{"RESTIC_CACHE_DIR": f.cache}, passFile),
	}, nil)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if snap.exitCode != 0 {
		t.Fatalf("restic snapshots exited %d: %s", snap.exitCode, snap.tail(5))
	}
	raw := strings.Join(snap.stdout, "\n")
	if strings.Contains(raw, older) {
		t.Errorf("forgotten snapshot still present in the repository: %q", older)
	}
	if !strings.Contains(raw, newest) {
		t.Errorf("newest snapshot missing after the forget: %q", newest)
	}
}

// TestForgetAndBackupSerializeOnTargetKey proves a forget job and a backup job
// of the same target cannot race: both carry backup.ConcurrencyKey (the web
// enqueues the forget under exactly that key), and the queue refuses to run a
// second job while the first holds the key. The backup leg is slowed through a
// fake engine and the forget leg through a blocking driver, so any overlap
// shows up as interleaved markers.
func TestForgetAndBackupSerializeOnTargetKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := f.target(t, "shared", KindResticLocal, filepath.Join(t.TempDir(), "repo"))
	older := fullID("a")
	seedRun(t, f.store, target.ID, older, 1)
	seedRun(t, f.store, target.ID, fullID("b"), 2)

	var mu sync.Mutex
	var markers []string
	mark := func(s string) {
		mu.Lock()
		markers = append(markers, s)
		mu.Unlock()
	}
	slowBackup := func(_ context.Context, targets []Target) (Report, error) {
		mark("start")
		time.Sleep(50 * time.Millisecond)
		mark("end")
		return Report{Legs: []LegResult{
			{Target: targets[0].Name, Kind: targets[0].Kind, State: store.BackupSucceeded},
		}}, nil
	}
	slowForget := &blockingDriver{mark: mark}
	runner := &Runner{store: f.store, run: slowBackup, env: f.env, stageRoot: "", driver: slowForget, targetEnv: nil}
	q := queue.New(f.store, runner, queue.Options{Workers: 4, PollInterval: 10 * time.Millisecond})

	if _, err := q.Enqueue(t.Context(), queue.Spec{
		Runbook: "backup", Kind: JobKind, Trigger: "manual",
		Argv: Argv(target.Name), ConcurrencyKey: ConcurrencyKey(target.Name),
	}); err != nil {
		t.Fatalf("enqueue backup: %v", err)
	}
	if _, err := q.Enqueue(t.Context(), queue.Spec{
		Runbook: "backup-forget", Kind: ForgetJobKind, Trigger: "web",
		Argv: ForgetArgv(target.Name, older), ConcurrencyKey: ConcurrencyKey(target.Name),
	}); err != nil {
		t.Fatalf("enqueue forget: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := q.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(markers) != 4 {
		t.Fatalf("markers = %v, want 4 entries", markers)
	}
	for i, m := range markers {
		want := "start"
		if i%2 == 1 {
			want = "end"
		}
		if m != want {
			t.Fatalf("markers = %v: entry %d is %q, want %q (the forget and backup overlapped)", markers, i, m, want)
		}
	}

	jobs, err := f.store.ListJobs(t.Context(), store.JobFilter{ConcurrencyKey: ConcurrencyKey(target.Name)})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, j := range jobs {
		if j.State != store.StateSucceeded {
			t.Errorf("job %d state = %s, want succeeded", j.ID, j.State)
		}
	}
}

// blockingDriver holds a concurrency key for a fixed span, so a serialization
// test can tell whether a second job started while the first was still in it.
type blockingDriver struct {
	mark func(string)
}

func (d *blockingDriver) Run(_ context.Context, _ executor.Spec, _ executor.EventSink) (executor.Result, error) {
	d.mark("start")
	time.Sleep(50 * time.Millisecond)
	d.mark("end")
	return executor.Result{ExitCode: 0}, nil
}
