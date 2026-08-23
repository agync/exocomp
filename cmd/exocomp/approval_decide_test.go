package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/policy"
	"exocomp.dev/exocomp/internal/store"
)

// seedPendingApproval routes a require_approval plan through `workspace apply
// --plan`, the same local, no-daemon path TestApplyGatesRequireApprovalPlan
// exercises, and returns the pending approval it created. It is the cheapest
// way to a real approval row: real TokenHash, real gated job, no daemon.
func seedPendingApproval(t *testing.T, ws string) (*store.Store, store.Approval) {
	t.Helper()
	st, artifactID := seedPlan(t, ws)
	ctx := context.Background()

	art, err := st.GetArtifact(ctx, artifactID)
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	if err := recordPlanVerdict(ctx, st, &strings.Builder{}, art.JobID, art.ID,
		policy.Verdict{Policy: "gate-changes", Action: store.PolicyRequireApproval}); err != nil {
		t.Fatalf("recordPlanVerdict: %v", err)
	}

	if out, err := runCobra(t, newWorkspaceCmd, "apply", ws, "--plan", strconv.FormatInt(artifactID, 10)); err != nil {
		t.Fatalf("gated apply: %v (%s)", err, out)
	}

	pending, err := st.ListApprovals(ctx, true)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("gated apply created %d pending approvals, want 1", len(pending))
	}
	return st, pending[0]
}

// TestApprovalDecideApprove proves --approve routes through the real
// fail-closed seam: it releases the gated job to queued and reports the
// outcome.
func TestApprovalDecideApprove(t *testing.T) {
	initForKit(t)
	st, approval := seedPendingApproval(t, "homelab")
	ctx := context.Background()

	out, err := runCobra(t, newApprovalCmd, "decide", strconv.FormatInt(approval.ID, 10), "--approve")
	if err != nil {
		t.Fatalf("approval decide --approve: %v (%s)", err, out)
	}
	if !strings.Contains(out, "approved") {
		t.Fatalf("decide output = %q, want it to say approved", out)
	}

	decided, err := st.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if decided.State != store.ApprovalApproved {
		t.Fatalf("approval state = %s, want approved", decided.State)
	}
	job, err := st.GetJob(ctx, approval.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != store.StateQueued {
		t.Fatalf("released job state = %s, want queued", job.State)
	}
}

// TestApprovalDecideReject proves --reject cancels the gated job rather than
// releasing it.
func TestApprovalDecideReject(t *testing.T) {
	initForKit(t)
	st, approval := seedPendingApproval(t, "homelab")
	ctx := context.Background()

	out, err := runCobra(t, newApprovalCmd, "decide", strconv.FormatInt(approval.ID, 10), "--reject")
	if err != nil {
		t.Fatalf("approval decide --reject: %v (%s)", err, out)
	}
	if !strings.Contains(out, "rejected") {
		t.Fatalf("decide output = %q, want it to say rejected", out)
	}

	decided, err := st.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if decided.State != store.ApprovalRejected {
		t.Fatalf("approval state = %s, want rejected", decided.State)
	}
	job, err := st.GetJob(ctx, approval.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != store.StateCancelled {
		t.Fatalf("rejected job state = %s, want cancelled", job.State)
	}
}

// TestApprovalDecideAlreadyDecidedFailsClosed proves a second decision on the
// same id changes nothing further and fails closed (nonzero exit), matching
// the invariant a used decision link already honors.
func TestApprovalDecideAlreadyDecidedFailsClosed(t *testing.T) {
	initForKit(t)
	st, approval := seedPendingApproval(t, "homelab")
	ctx := context.Background()

	if out, err := runCobra(t, newApprovalCmd, "decide", strconv.FormatInt(approval.ID, 10), "--approve"); err != nil {
		t.Fatalf("first decide: %v (%s)", err, out)
	}
	beforeJob, err := st.GetJob(ctx, approval.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	out, err := runCobra(t, newApprovalCmd, "decide", strconv.FormatInt(approval.ID, 10), "--reject")
	if err == nil {
		t.Fatalf("a second decision on an already-decided approval succeeded: %s", out)
	}
	if !strings.Contains(err.Error(), "already been used") {
		t.Fatalf("second-decide error = %v, want it to name the used link", err)
	}

	// Idempotent: the settled job did not move again (a reject after an
	// approve must not cancel the already-released job).
	afterJob, err := st.GetJob(ctx, approval.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if afterJob.State != beforeJob.State {
		t.Fatalf("job moved from %s to %s on the losing decide", beforeJob.State, afterJob.State)
	}
}

// TestApprovalDecideUnknownIDFailsClosed proves an id naming no approval fails
// closed with a clear error rather than a panic or a silent no-op.
func TestApprovalDecideUnknownIDFailsClosed(t *testing.T) {
	initForKit(t)

	out, err := runCobra(t, newApprovalCmd, "decide", "999999", "--approve")
	if err == nil {
		t.Fatalf("decide on an unknown id succeeded: %s", out)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown-id error = %v, want it to say not found", err)
	}
}

// TestApprovalDecideRequiresExactlyOneFlag proves neither flag and both flags
// are both refused, so the CLI never guesses a decision.
func TestApprovalDecideRequiresExactlyOneFlag(t *testing.T) {
	initForKit(t)
	_, approval := seedPendingApproval(t, "homelab")
	id := strconv.FormatInt(approval.ID, 10)

	if out, err := runCobra(t, newApprovalCmd, "decide", id); err == nil {
		t.Fatalf("decide with neither flag succeeded: %s", out)
	}
	if out, err := runCobra(t, newApprovalCmd, "decide", id, "--approve", "--reject"); err == nil {
		t.Fatalf("decide with both flags succeeded: %s", out)
	}
}

// TestApprovalDecideJSON proves --output json emits the decision through the
// wire success envelope on a real decision, and the wire error envelope on a
// fail-closed one.
func TestApprovalDecideJSON(t *testing.T) {
	initForKit(t)
	_, approval := seedPendingApproval(t, "homelab")
	id := strconv.FormatInt(approval.ID, 10)

	out, err := run(t, "approval", "decide", id, "--approve", "--output", "json")
	if err != nil {
		t.Fatalf("json decide: %v (%s)", err, out)
	}
	var success struct {
		SchemaVersion int    `json:"schema_version"`
		Kind          string `json:"kind"`
		Data          struct {
			ApprovalID int64  `json:"approval_id"`
			Outcome    string `json:"outcome"`
			Message    string `json:"message"`
		} `json:"data"`
	}
	if jerr := json.Unmarshal([]byte(out), &success); jerr != nil {
		t.Fatalf("decode success envelope: %v (%s)", jerr, out)
	}
	if success.Kind != "approval.decision" {
		t.Fatalf("kind = %q, want approval.decision", success.Kind)
	}
	if success.Data.ApprovalID != approval.ID || success.Data.Outcome != "approved" {
		t.Fatalf("data = %+v, want approval %d approved", success.Data, approval.ID)
	}

	out, err = run(t, "approval", "decide", id, "--reject", "--output", "json")
	if err == nil {
		t.Fatalf("re-deciding a settled approval succeeded: %s", out)
	}
	var failure struct {
		Kind  string `json:"kind"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if jerr := json.Unmarshal([]byte(out), &failure); jerr != nil {
		t.Fatalf("decode error envelope: %v (%s)", jerr, out)
	}
	if failure.Kind != "error" || failure.Error.Code != "conflict" {
		t.Fatalf("failure envelope = %+v, want kind error / code conflict", failure)
	}
}
