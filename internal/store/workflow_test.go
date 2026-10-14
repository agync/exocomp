package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func openWorkflowStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newJobID inserts a throwaway job and returns its id, so a step's job_id
// satisfies the foreign key the ledger enforces.
func newJobID(t *testing.T, s *Store) int64 {
	t.Helper()
	job, err := s.CreateJob(context.Background(), NewJob{Runbook: "wf", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	return job.ID
}

// twoStepLedger creates a workflow of two pending steps and returns its id.
func twoStepLedger(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	id := "wf-test"
	steps := []NewWorkflowStep{
		{StepNo: 1, Kind: "shell", IdempotencyKey: "wf-test:1"},
		{StepNo: 2, Kind: "shell", IdempotencyKey: "wf-test:2"},
	}
	if err := s.CreateWorkflow(ctx, id, steps); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	return id
}

func TestCreateWorkflowStartsAllPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWorkflowStore(t)
	id := twoStepLedger(t, s)

	steps, err := s.GetWorkflow(ctx, id)
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(steps))
	}
	for i, step := range steps {
		if step.StepNo != i+1 {
			t.Errorf("step %d has step_no %d", i, step.StepNo)
		}
		if step.Status != StepPending {
			t.Errorf("step %d is %s, want pending", step.StepNo, step.Status)
		}
	}
	if got := WorkflowStatus(steps); got != WorkflowRunning {
		t.Errorf("workflow status = %s, want running", got)
	}
}

func TestGetWorkflowNotFound(t *testing.T) {
	t.Parallel()
	s := openWorkflowStore(t)
	if _, err := s.GetWorkflow(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStepTransitionsAndWorkflowStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWorkflowStore(t)
	id := twoStepLedger(t, s)
	job1 := newJobID(t, s)

	if err := s.StartStep(ctx, id, 1, job1); err != nil {
		t.Fatalf("start step 1: %v", err)
	}
	if err := s.CompleteStep(ctx, id, 1, StepSucceeded, `{"exit_code":0}`); err != nil {
		t.Fatalf("complete step 1: %v", err)
	}

	steps, err := s.GetWorkflow(ctx, id)
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	if steps[0].Status != StepSucceeded {
		t.Errorf("step 1 is %s, want succeeded", steps[0].Status)
	}
	if steps[0].JobID == nil || *steps[0].JobID != job1 {
		t.Errorf("step 1 job_id = %v, want %d", steps[0].JobID, job1)
	}
	if got := WorkflowStatus(steps); got != WorkflowRunning {
		t.Errorf("status after one success = %s, want running", got)
	}

	// Second step fails; the workflow becomes failed and stays readable.
	if err := s.StartStep(ctx, id, 2, newJobID(t, s)); err != nil {
		t.Fatalf("start step 2: %v", err)
	}
	if err := s.CompleteStep(ctx, id, 2, StepFailed, `{"exit_code":1}`); err != nil {
		t.Fatalf("complete step 2: %v", err)
	}
	steps, _ = s.GetWorkflow(ctx, id)
	if got := WorkflowStatus(steps); got != WorkflowFailed {
		t.Errorf("status after a failure = %s, want failed", got)
	}
}

// TestStartStepGuardRejectsDoubleStart proves the pending guard: a step already
// running cannot be started again, so two drivers cannot both claim it.
func TestStartStepGuardRejectsDoubleStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWorkflowStore(t)
	id := twoStepLedger(t, s)

	if err := s.StartStep(ctx, id, 1, newJobID(t, s)); err != nil {
		t.Fatalf("start step 1: %v", err)
	}
	err := s.StartStep(ctx, id, 1, newJobID(t, s))
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("second start err = %v, want ErrStateConflict", err)
	}
}

// TestCompleteStepRejectsNonRunning proves a step cannot be completed unless it
// is running: the guard is what makes a duplicate completion a no-op conflict
// rather than a silent second effect.
func TestCompleteStepRejectsNonRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWorkflowStore(t)
	id := twoStepLedger(t, s)

	// Step 1 is still pending, never started.
	err := s.CompleteStep(ctx, id, 1, StepSucceeded, "")
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("complete of a pending step err = %v, want ErrStateConflict", err)
	}
}

func TestBlockPendingStepsOnlyTouchesPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWorkflowStore(t)
	id := twoStepLedger(t, s)

	if err := s.StartStep(ctx, id, 1, newJobID(t, s)); err != nil {
		t.Fatalf("start step 1: %v", err)
	}
	if err := s.CompleteStep(ctx, id, 1, StepFailed, ""); err != nil {
		t.Fatalf("fail step 1: %v", err)
	}

	blocked, err := s.BlockPendingSteps(ctx, id)
	if err != nil {
		t.Fatalf("block: %v", err)
	}
	if blocked != 1 {
		t.Fatalf("blocked %d steps, want 1 (only the pending step 2)", blocked)
	}
	steps, _ := s.GetWorkflow(ctx, id)
	if steps[0].Status != StepFailed {
		t.Errorf("step 1 is %s, want failed (unchanged by block)", steps[0].Status)
	}
	if steps[1].Status != StepBlocked {
		t.Errorf("step 2 is %s, want blocked", steps[1].Status)
	}
}

func TestListIncompleteWorkflows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWorkflowStore(t)

	// A finished workflow: both steps succeeded.
	if err := s.CreateWorkflow(ctx, "done", []NewWorkflowStep{{StepNo: 1, Kind: "shell", IdempotencyKey: "done:1"}}); err != nil {
		t.Fatalf("create done: %v", err)
	}
	if err := s.StartStep(ctx, "done", 1, newJobID(t, s)); err != nil {
		t.Fatalf("start done: %v", err)
	}
	if err := s.CompleteStep(ctx, "done", 1, StepSucceeded, ""); err != nil {
		t.Fatalf("complete done: %v", err)
	}

	// A failed workflow: step failed, rest blocked — terminal, so not incomplete.
	if err := s.CreateWorkflow(ctx, "failed", []NewWorkflowStep{
		{StepNo: 1, Kind: "shell", IdempotencyKey: "failed:1"},
		{StepNo: 2, Kind: "shell", IdempotencyKey: "failed:2"},
	}); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if err := s.StartStep(ctx, "failed", 1, newJobID(t, s)); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if err := s.CompleteStep(ctx, "failed", 1, StepFailed, ""); err != nil {
		t.Fatalf("fail failed: %v", err)
	}
	if _, err := s.BlockPendingSteps(ctx, "failed"); err != nil {
		t.Fatalf("block failed: %v", err)
	}

	// A live workflow with a pending step.
	twoStepLedger(t, s)

	ids, err := s.ListIncompleteWorkflows(ctx)
	if err != nil {
		t.Fatalf("list incomplete: %v", err)
	}
	if len(ids) != 1 || ids[0] != "wf-test" {
		t.Fatalf("incomplete = %v, want [wf-test]", ids)
	}
}

// TestCompleteStepTxAtomicWithEffect is the same-transaction gate (Decision 0008):
// a step's own effect and its ledger row commit or roll back together, so a
// crash between them cannot land the effect while the ledger still calls the
// step running — the state a resume would re-run. The effect stands in as an
// insert into artifact.
func TestCompleteStepTxAtomicWithEffect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWorkflowStore(t)
	id := twoStepLedger(t, s)

	if err := s.StartStep(ctx, id, 1, newJobID(t, s)); err != nil {
		t.Fatalf("start step 1: %v", err)
	}

	// A job row for the artifact FK to reference.
	job, err := s.CreateJob(ctx, NewJob{Runbook: "wf", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	effect := func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO artifact (job_id, kind, path, sha256, size) VALUES (?, 'output', '/x', '', 0)`,
			job.ID)
		return err
	}

	// Rollback path: the effect and the ledger write both happen, then fn
	// returns an error, so neither must persist.
	wantErr := errors.New("boom")
	err = s.InTx(ctx, func(tx *sql.Tx) error {
		if err := effect(tx); err != nil {
			return err
		}
		if err := s.CompleteStepTx(ctx, tx, id, 1, StepSucceeded, ""); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("InTx err = %v, want boom", err)
	}
	step, _ := s.GetWorkflowStep(ctx, id, 1)
	if step.Status != StepRunning {
		t.Fatalf("after rollback step is %s, want running (ledger write rolled back)", step.Status)
	}
	arts, _ := s.ListArtifacts(ctx, job.ID)
	if len(arts) != 0 {
		t.Fatalf("after rollback %d artifacts persisted, want 0 (effect rolled back)", len(arts))
	}

	// Commit path: both land.
	if err := s.InTx(ctx, func(tx *sql.Tx) error {
		if err := effect(tx); err != nil {
			return err
		}
		return s.CompleteStepTx(ctx, tx, id, 1, StepSucceeded, `{"exit_code":0}`)
	}); err != nil {
		t.Fatalf("commit InTx: %v", err)
	}
	step, _ = s.GetWorkflowStep(ctx, id, 1)
	if step.Status != StepSucceeded {
		t.Fatalf("after commit step is %s, want succeeded", step.Status)
	}
	arts, _ = s.ListArtifacts(ctx, job.ID)
	if len(arts) != 1 {
		t.Fatalf("after commit %d artifacts, want 1", len(arts))
	}
}

// TestWorkflowStepSchema pins the migration's columns so a later change to the
// ledger shape is a deliberate one.
func TestWorkflowStepSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWorkflowStore(t)

	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(workflow_step)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()

	got := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = true
	}
	for _, col := range []string{
		"workflow_id", "step_no", "kind", "status", "idempotency_key",
		"job_id", "result_json", "rollback_marker", "created_at", "updated_at",
	} {
		if !got[col] {
			t.Errorf("workflow_step is missing column %q", col)
		}
	}
}
