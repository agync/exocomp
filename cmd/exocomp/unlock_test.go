package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// takeLock records a held tofu state lock on a workspace, naming the job that
// took it. It is what the state backend writes when tofu locks.
func takeLock(t *testing.T, workspace string, jobID *int64) store.TFLock {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	ws, err := st.WorkspaceByName(context.Background(), workspace)
	if err != nil {
		t.Fatalf("workspace %s: %v", workspace, err)
	}
	held, err := st.AcquireTFLock(context.Background(), store.TFLock{
		WorkspaceID: ws.ID,
		LockID:      "lock-under-test",
		Who:         "operator@host",
		Operation:   "OperationTypeApply",
		JobID:       jobID,
	})
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	return held
}

func lockedWorkspace(t *testing.T, workspace string) bool {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	ws, err := st.WorkspaceByName(context.Background(), workspace)
	if err != nil {
		t.Fatalf("workspace %s: %v", workspace, err)
	}
	_, err = st.GetTFLock(context.Background(), ws.ID)
	return err == nil
}

// tryLock asks the running state backend for the workspace lock, the way tofu
// does, and returns the status code. It is the seam an apply actually meets.
func tryLock(t *testing.T, addr, workspace, lockID string) int {
	t.Helper()

	dir, err := dataDir()
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()

	body, err := json.Marshal(map[string]string{
		"ID": lockID, "Who": "test@host", "Operation": "OperationTypeApply",
	})
	if err != nil {
		t.Fatalf("encode lock body: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "LOCK",
		api.StateURL(addr, workspace), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build lock request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+api.WorkspaceToken(env, workspace))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("LOCK %s: %v", workspace, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestStaleStateLockIsFlaggedAndBreakable is the recovery ADR 0004 promises
// and nothing implemented: a job killed mid-apply leaves the workspace locked
// by a process that is gone, and no later run can ever take that lock.
func TestStaleStateLockIsFlaggedAndBreakable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	bin := buildExocomp(t)

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)

	// A real killed process: its job row stays running, owned by a pid that
	// no longer exists, and the lock it took stays held.
	abandoned := abandonJob(t)
	takeLock(t, "wsp", &abandoned)

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	line := d.waitForLine(t, &d.stdout, "state backend listening on http://", 30*time.Second)
	addr := strings.TrimPrefix(strings.TrimSpace(line), "state backend listening on http://")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	if !strings.Contains(d.stderr.String(), "stale state lock") {
		t.Errorf("the boot sweep did not flag the stale lock:\n%s", d.dump())
	}
	if got := tryLock(t, addr, "wsp", "would-be-apply"); got != http.StatusLocked {
		t.Fatalf("LOCK before the unlock = %d, want 423 (the stale lock is not holding)", got)
	}

	out, code := runCLIExit(t, bin, "workspace", "unlock", "wsp")
	if code != 0 {
		t.Fatalf("workspace unlock exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "unlocked") || !strings.Contains(out, fmt.Sprintf("job %d", abandoned)) {
		t.Errorf("workspace unlock did not report the break and its audit row:\n%s", out)
	}
	if lockedWorkspace(t, "wsp") {
		t.Error("the lock survived the unlock")
	}
	if events := jobEventText(t, abandoned); !strings.Contains(events, "force-unlocked") {
		t.Errorf("the unlock was not audited on job %d:\n%s", abandoned, events)
	}

	// An apply can take the lock again, which is the whole point.
	if got := tryLock(t, addr, "wsp", "next-apply"); got != http.StatusOK {
		t.Errorf("LOCK after the unlock = %d, want 200", got)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
}

// TestWorkspaceUnlockRefusesALiveJob: a lock held by a job that is still
// running is the system working. Breaking it would let a second apply write
// state under the first one.
func TestWorkspaceUnlockRefusesALiveJob(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	module := t.TempDir()
	if _, err := runWorkspaceCmd(t, "add", "wsp", "--dir", module); err != nil {
		t.Fatalf("workspace add: %v", err)
	}

	live := runningJob(t)
	takeLock(t, "wsp", &live)

	out, err := runWorkspaceCmd(t, "unlock", "wsp")
	if err == nil {
		t.Fatalf("unlock broke a live job's lock:\n%s", out)
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("error = %v, want one naming the running job", err)
	}
	if !lockedWorkspace(t, "wsp") {
		t.Error("the refused unlock released the lock anyway")
	}
}

func TestWorkspaceUnlockOnAFreeWorkspace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	module := t.TempDir()
	if _, err := runWorkspaceCmd(t, "add", "wsp", "--dir", module); err != nil {
		t.Fatalf("workspace add: %v", err)
	}

	out, err := runWorkspaceCmd(t, "unlock", "wsp")
	if err != nil {
		t.Fatalf("unlock of a free workspace: %v", err)
	}
	if !strings.Contains(out, "is not locked") {
		t.Errorf("output = %q, want it to say the workspace is not locked", out)
	}
}

// runningJob creates a job this process owns and moves it to running, so the
// lock below names an owner that is demonstrably alive.
func runningJob(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	job, err := st.CreateJob(ctx, store.NewJob{
		Runbook: workspaceRunbookPrefix + "wsp",
		Kind:    "tofu",
		Trigger: "manual",
		Args:    store.JobArgs{Argv: []string{"tofu", "apply"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := st.ClaimJob(ctx, job.ID); err != nil {
		t.Fatalf("claim job: %v", err)
	}
	return job.ID
}

func runWorkspaceCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	cmd := newWorkspaceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)

	// Execute first: a return statement evaluates its operands left to right,
	// so reading the buffer in the return would read it before the command ran.
	err := cmd.Execute()
	return out.String(), err
}
