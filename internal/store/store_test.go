package store_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exocomp.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s, path
}

func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	first, path := open(t)
	version, err := first.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version == 0 {
		t.Fatal("schema version is 0 after migrate")
	}
	if _, err := first.CreateJob(ctx, store.NewJob{Runbook: "keep", Kind: "shell", Trigger: "manual"}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }()

	again, err := second.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if again != version {
		t.Fatalf("schema version changed on reopen: %d -> %d", version, again)
	}
	jobs, err := second.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Runbook != "keep" {
		t.Fatalf("reopen lost data: %+v", jobs)
	}
}

func TestOpenSetsPragmas(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	for _, tc := range []struct{ pragma, want string }{
		{"journal_mode", "wal"},
		{"busy_timeout", "5000"},
		{"synchronous", "1"},
		{"foreign_keys", "1"},
	} {
		got, err := s.Pragma(ctx, tc.pragma)
		if err != nil {
			t.Fatalf("pragma %s: %v", tc.pragma, err)
		}
		if got != tc.want {
			t.Errorf("pragma %s = %q, want %q", tc.pragma, got, tc.want)
		}
	}
}

func TestCreateJobRoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	job, err := s.CreateJob(ctx, store.NewJob{
		Runbook:        "site.yml",
		Kind:           "shell",
		Trigger:        "manual",
		Args:           store.JobArgs{Argv: []string{"/bin/true"}, Dir: "/tmp", Timeout: store.Duration(30 * time.Second)},
		ConcurrencyKey: "workspace-a",
		DedupKey:       "dedup-1",
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	got, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != store.StateQueued {
		t.Errorf("state = %q, want queued", got.State)
	}
	if got.OwnerPID != os.Getpid() {
		t.Errorf("owner pid = %d, want %d", got.OwnerPID, os.Getpid())
	}
	if len(got.Args.Argv) != 1 || got.Args.Argv[0] != "/bin/true" {
		t.Errorf("argv = %v", got.Args.Argv)
	}
	if time.Duration(got.Args.Timeout) != 30*time.Second {
		t.Errorf("timeout = %v", time.Duration(got.Args.Timeout))
	}
	if got.StartedAt != nil || got.FinishedAt != nil || got.ExitCode != nil {
		t.Errorf("fresh job has start/finish/exit set: %+v", got)
	}
	if got.DedupKey != "dedup-1" || got.IdempotencyKey != "idem-1" {
		t.Errorf("keys = %q/%q", got.DedupKey, got.IdempotencyKey)
	}
}

func TestIdempotencyKeyIsUnique(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	spec := store.NewJob{Runbook: "r", Kind: "shell", Trigger: "api", IdempotencyKey: "once"}
	first, err := s.CreateJob(ctx, spec)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := s.CreateJob(ctx, spec); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("second create err = %v, want ErrDuplicate", err)
	}

	found, err := s.JobByIdempotencyKey(ctx, "once")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found.ID != first.ID {
		t.Fatalf("lookup returned job %d, want %d", found.ID, first.ID)
	}

	// Jobs without a key must not collide on NULL.
	for range 2 {
		if _, err := s.CreateJob(ctx, store.NewJob{Runbook: "r", Kind: "shell", Trigger: "manual"}); err != nil {
			t.Fatalf("create keyless job: %v", err)
		}
	}
}

func TestTransitionJob(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	job, err := s.CreateJob(ctx, store.NewJob{Runbook: "r", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	if err := s.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("queued -> running: %v", err)
	}
	running, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if running.StartedAt == nil {
		t.Error("running job has no started_at")
	}

	if err := s.SetJobExitCode(ctx, job.ID, 0); err != nil {
		t.Fatalf("set exit code: %v", err)
	}
	if err := s.TransitionJob(ctx, job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("running -> succeeded: %v", err)
	}
	done, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if done.FinishedAt == nil {
		t.Error("terminal job has no finished_at")
	}
	if done.ExitCode == nil || *done.ExitCode != 0 {
		t.Errorf("exit code = %v", done.ExitCode)
	}
	if done.OwnerPID != 0 {
		t.Errorf("terminal job still owned by pid %d", done.OwnerPID)
	}
}

func TestTransitionJobRejectsIllegalMoves(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	illegal := []struct{ from, to store.JobState }{
		{store.StateQueued, store.StateSucceeded},
		{store.StateQueued, store.StateFailed},
		{store.StateQueued, store.StateTimedOut},
		{store.StateRunning, store.StateQueued},
		{store.StateSucceeded, store.StateRunning},
		{store.StateFailed, store.StateQueued},
		{store.StateInterrupted, store.StateRunning},
		{store.StateCancelled, store.StateSucceeded},
		{store.StateTimedOut, store.StateFailed},
	}
	for _, tc := range illegal {
		job, err := s.CreateJob(ctx, store.NewJob{Runbook: "r", Kind: "shell", Trigger: "manual"})
		if err != nil {
			t.Fatalf("create job: %v", err)
		}
		err = s.TransitionJob(ctx, job.ID, tc.from, tc.to)
		if !errors.Is(err, store.ErrIllegalTransition) {
			t.Errorf("%s -> %s: err = %v, want ErrIllegalTransition", tc.from, tc.to, err)
		}
		after, err := s.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if after.State != store.StateQueued {
			t.Errorf("%s -> %s changed state to %s", tc.from, tc.to, after.State)
		}
	}
}

func TestTransitionJobDetectsStateConflict(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	job, err := s.CreateJob(ctx, store.NewJob{Runbook: "r", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("claim: %v", err)
	}
	err = s.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning)
	if !errors.Is(err, store.ErrStateConflict) {
		t.Fatalf("second claim err = %v, want ErrStateConflict", err)
	}
}

func TestSweepInterrupted(t *testing.T) {
	t.Parallel()
	s, path := open(t)
	ctx := context.Background()

	orphanRunning, err := s.CreateJob(ctx, store.NewJob{Runbook: "orphan", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.TransitionJob(ctx, orphanRunning.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("claim: %v", err)
	}
	orphanQueued, err := s.CreateJob(ctx, store.NewJob{Runbook: "orphan-queued", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	done, err := s.CreateJob(ctx, store.NewJob{Runbook: "done", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.TransitionJob(ctx, done.ID, store.StateQueued, store.StateCancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Rows owned by this live process are not orphans.
	swept, err := s.SweepInterrupted(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 0 {
		t.Fatalf("sweep touched %d live rows, want 0", swept)
	}

	// Reassign ownership to a pid this process cannot be, i.e. the previous
	// exocompd, then sweep as a fresh boot would.
	orphan(t, path, orphanRunning.ID, orphanQueued.ID)

	swept, err = s.SweepInterrupted(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 2 {
		t.Fatalf("swept %d rows, want 2", swept)
	}
	for _, id := range []int64{orphanRunning.ID, orphanQueued.ID} {
		job, err := s.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job.State != store.StateInterrupted {
			t.Errorf("job %d is %s, want interrupted", id, job.State)
		}
		if job.FinishedAt == nil {
			t.Errorf("job %d interrupted without finished_at", id)
		}
	}
	stillCancelled, err := s.GetJob(ctx, done.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stillCancelled.State != store.StateCancelled {
		t.Errorf("terminal job became %s", stillCancelled.State)
	}
}

// orphan rewrites owner_pid on the given jobs to a pid that has genuinely
// exited, simulating rows left by a killed exocompd. A pid that merely differs
// from ours is not enough: the sweep asks the kernel whether the process is
// alive, and an arbitrary number often belongs to something that is.
func orphan(t *testing.T, path string, ids ...int64) {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open for orphaning: %v", err)
	}
	defer func() { _ = s.Close() }()

	dead := deadPID(t)
	for _, id := range ids {
		if err := s.SetOwnerPIDForTest(id, dead); err != nil {
			t.Fatalf("orphan job %d: %v", id, err)
		}
	}
}

// deadPID runs a process to completion and reaps it, so its pid names nothing.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run /bin/true: %v", err)
	}
	return cmd.Process.Pid
}

// livePID starts a process that outlives the test body, so its pid names
// something the sweep must leave alone.
func livePID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start /bin/sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

// TestSweepInterruptedUsesLivenessNotPIDEquality pins the two cases pid
// equality gets wrong. A container restart reissues pid 1, so a row this
// process "owns" can predate it; and another live process's rows are not ours
// to terminate.
func TestSweepInterruptedUsesLivenessNotPIDEquality(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	// The reused-pid case: same owner_pid as us, stamped before we started.
	restarted, err := s.CreateJob(ctx, store.NewJob{Runbook: "before-restart", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.TransitionJob(ctx, restarted.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.BackdateJobForTest(restarted.ID, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// The live-neighbour case: a pid that is not ours and is running.
	neighbour, err := s.CreateJob(ctx, store.NewJob{Runbook: "neighbour", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.SetOwnerPIDForTest(neighbour.ID, livePID(t)); err != nil {
		t.Fatalf("reassign owner: %v", err)
	}

	// Ours, started in this process: untouchable.
	mine, err := s.CreateJob(ctx, store.NewJob{Runbook: "mine", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	swept, err := s.SweepInterrupted(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept %d rows, want 1 (the row left by the previous process at this pid)", swept)
	}

	for id, want := range map[int64]store.JobState{
		restarted.ID: store.StateInterrupted,
		neighbour.ID: store.StateQueued,
		mine.ID:      store.StateQueued,
	} {
		job, err := s.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("get job %d: %v", id, err)
		}
		if job.State != want {
			t.Errorf("job %d (%s) is %s, want %s", id, job.Runbook, job.State, want)
		}
	}
}

func TestJobEventsAreAppendOnly(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	job, err := s.CreateJob(ctx, store.NewJob{Runbook: "r", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	for i, msg := range []string{"one", "two", "three"} {
		ev, err := s.AppendJobEvent(ctx, store.JobEvent{JobID: job.ID, Msg: msg})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq = %d, want %d", ev.Seq, i+1)
		}
	}

	events, err := s.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 3 || events[0].Msg != "one" || events[2].Msg != "three" {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Level != store.LevelInfo {
		t.Errorf("default level = %q", events[0].Level)
	}

	last, err := s.LastJobEvents(ctx, job.ID, 2)
	if err != nil {
		t.Fatalf("last events: %v", err)
	}
	if len(last) != 2 || last[0].Msg != "two" || last[1].Msg != "three" {
		t.Fatalf("last events = %+v", last)
	}

	if err := s.UpdateJobEventForTest(job.ID, 1, "rewritten"); err == nil {
		t.Fatal("updating a job_event succeeded, want the append-only trigger to abort it")
	}
}

func TestArtifactCRUD(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	job, err := s.CreateJob(ctx, store.NewJob{Runbook: "r", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	created, err := s.CreateArtifact(ctx, store.Artifact{
		JobID: job.ID, Kind: "log", Path: "/var/log/job-1.log", SHA256: "abc", Size: 42,
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	got, err := s.GetArtifact(ctx, created.ID)
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	if got.Size != 42 || got.Kind != "log" {
		t.Fatalf("artifact = %+v", got)
	}
	list, err := s.ListArtifacts(ctx, job.ID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d artifacts, want 1", len(list))
	}
	if err := s.DeleteArtifact(ctx, created.ID); err != nil {
		t.Fatalf("delete artifact: %v", err)
	}
	if _, err := s.GetArtifact(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if _, err := s.CreateArtifact(ctx, store.Artifact{JobID: 9999, Kind: "log", Path: "x"}); err == nil {
		t.Fatal("artifact for a missing job was accepted, want a foreign key error")
	}
}

func TestKeyringCRUD(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	entry, err := s.CreateKeyringEntry(ctx, store.KeyringEntry{
		Kind: "host", Recipient: "age1host", WrappedDEK: []byte{1, 2, 3}, Label: "host key",
	})
	if err != nil {
		t.Fatalf("create keyring entry: %v", err)
	}
	if _, err := s.CreateKeyringEntry(ctx, store.KeyringEntry{Kind: "extra", Recipient: "age1host"}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate recipient err = %v, want ErrDuplicate", err)
	}

	got, err := s.GetKeyringEntry(ctx, "age1host")
	if err != nil {
		t.Fatalf("get keyring entry: %v", err)
	}
	if string(got.WrappedDEK) != "\x01\x02\x03" {
		t.Fatalf("wrapped dek = %v", got.WrappedDEK)
	}

	if err := s.RevokeKeyringEntry(ctx, entry.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	active, err := s.ListKeyringEntries(ctx, false)
	if err != nil {
		t.Fatalf("list keyring: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("revoked entry still active: %+v", active)
	}
	all, err := s.ListKeyringEntries(ctx, true)
	if err != nil {
		t.Fatalf("list keyring: %v", err)
	}
	if len(all) != 1 || all[0].RevokedAt == nil {
		t.Fatalf("revoked entry = %+v", all)
	}
}

func TestSecretCRUD(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	if _, err := s.CreateSecret(ctx, store.SecretRow{
		Name: "vault_pass", Kind: "vault_pass",
		Nonce: []byte("nonce12bytes"), Ciphertext: []byte("sealed"), AAD: "vault_pass|vault_pass",
	}); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if _, err := s.CreateSecret(ctx, store.SecretRow{Name: "vault_pass", Kind: "token", Nonce: []byte("n"), Ciphertext: []byte("c")}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate name err = %v, want ErrDuplicate", err)
	}

	if err := s.RotateSecret(ctx, "vault_pass", []byte("nonce2"), []byte("sealed2")); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := s.TouchSecret(ctx, "vault_pass"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	got, err := s.GetSecret(ctx, "vault_pass")
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(got.Ciphertext) != "sealed2" || got.RotatedAt == nil || got.LastUsedAt == nil {
		t.Fatalf("secret = %+v", got)
	}

	list, err := s.ListSecrets(ctx)
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d secrets, want 1", len(list))
	}
	if err := s.DeleteSecret(ctx, "vault_pass"); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	if _, err := s.GetSecret(ctx, "vault_pass"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := s.RotateSecret(ctx, "missing", nil, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rotate missing: %v", err)
	}
}

func TestBackupCRUD(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()

	target, err := s.CreateBackupTarget(ctx, store.BackupTarget{
		Name: "local", Kind: "restic_local", URI: "/srv/restic", Retention: "7d", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := s.CreateBackupTarget(ctx, store.BackupTarget{Name: "local", Kind: "git", URI: "x"}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate name err = %v, want ErrDuplicate", err)
	}

	run, err := s.StartBackupRun(ctx, target.ID)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run.State = store.BackupSucceeded
	run.SnapshotID = "deadbeef"
	run.BytesAdded = 4096
	if err := s.FinishBackupRun(ctx, run); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	runs, err := s.ListBackupRuns(ctx, target.ID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].State != store.BackupSucceeded || runs[0].FinishedAt == nil {
		t.Fatalf("runs = %+v", runs)
	}
	if runs[0].BytesAdded != 4096 || runs[0].SnapshotID != "deadbeef" {
		t.Fatalf("run = %+v", runs[0])
	}

	target.Enabled = false
	if err := s.UpdateBackupTarget(ctx, target); err != nil {
		t.Fatalf("update target: %v", err)
	}
	enabled, err := s.ListBackupTargets(ctx, false)
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(enabled) != 0 {
		t.Fatalf("disabled target still listed: %+v", enabled)
	}

	if err := s.DeleteBackupTarget(ctx, target.ID); err != nil {
		t.Fatalf("delete target: %v", err)
	}
	remaining, err := s.ListBackupRuns(ctx, target.ID, 0)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("runs survived target deletion: %+v", remaining)
	}
}
