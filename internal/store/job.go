package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// JobState is a job's position in the execution state machine.
type JobState string

// The job states. awaiting_approval is the approval gate: a job in it is
// claimable by nothing, and only a decision moves it back to queued.
const (
	StateQueued           JobState = "queued"
	StateAwaitingApproval JobState = "awaiting_approval"
	StateRunning          JobState = "running"
	StateSucceeded        JobState = "succeeded"
	// StateDegraded is a terminal "succeeded with warnings": the run finished
	// and every reachable target converged, but one or more targets were
	// skipped as unreachable because the launch tolerated it. It is not a
	// failure and never pages; it still needs the operator, who must see that
	// the skipped hosts were never configured.
	StateDegraded    JobState = "degraded"
	StateFailed      JobState = "failed"
	StateCancelled   JobState = "cancelled"
	StateTimedOut    JobState = "timed_out"
	StateInterrupted JobState = "interrupted"
)

// jobTransitions is the whole state machine. States absent from the map are
// terminal.
var jobTransitions = map[JobState]map[JobState]bool{
	StateQueued: {
		StateRunning:          true,
		StateAwaitingApproval: true,
		StateCancelled:        true,
		StateInterrupted:      true,
	},
	StateAwaitingApproval: {
		StateQueued:      true,
		StateRunning:     true,
		StateCancelled:   true,
		StateInterrupted: true,
	},
	StateRunning: {
		StateSucceeded:   true,
		StateDegraded:    true,
		StateFailed:      true,
		StateCancelled:   true,
		StateTimedOut:    true,
		StateInterrupted: true,
	},
}

// Terminal reports whether the state admits no further transition.
func (s JobState) Terminal() bool {
	_, ok := jobTransitions[s]
	return !ok
}

// JobArgs is what a job needs to execute. It stands in for a future runbook
// table: for now a job carries its own entrypoint. Secret material never goes
// here — it is resolved at execution time and never written to the database.
type JobArgs struct {
	Argv []string `json:"argv,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	// DirReadOnly grants Dir read access only under the sandbox — how a
	// read-only path source (Source.Writable false) denies a run writes to its
	// tree. Off by default, so an existing job row without the field runs with a
	// writable working directory exactly as before.
	DirReadOnly bool     `json:"dir_read_only,omitempty"`
	Timeout     Duration `json:"timeout,omitempty"`
	// VaultProject names the envelope derivation an ansible job's vault
	// password comes from. The project name is public; the password it
	// derives is resolved when the job runs and never written here.
	VaultProject string `json:"vault_project,omitempty"`
	// RunSecretBinding names the run_secret set (a store binding) whose sealed
	// values are resolved to named env for this run alone — the credential
	// plane a provider or dynamic-inventory plugin authenticates from. Only the
	// binding name travels here; the secret values are resolved at execution
	// time and never written to the job row.
	RunSecretBinding string `json:"run_secret_binding,omitempty"`
	// Inventory names a registered inventory. For a dynamic inventory its
	// env_binding is resolved into the run's credential env alongside
	// RunSecretBinding. The name is public; the values it binds are not.
	Inventory string `json:"inventory,omitempty"`
	// Source names the registered source (ADR 0010) this run executed from,
	// when one was named. It is a display fact only — Dir already carries the
	// checkout path a claiming process needs — recorded so the job event
	// stream and every job face can show which source ran rather than an
	// absolute checkout path. Empty on a raw-path or ad-hoc run.
	Source string `json:"source,omitempty"`
	// ExtraVars are an ansible run's operator-supplied `-e key=val` variables.
	// They live on the row rather than in Argv so a daemon that claims the job
	// replays them, and they never reach the ansible process's own argv: the
	// driver writes them to a file in the run's private directory (AGENTS.md
	// invariant 4 — nothing that could carry a value belongs in a process
	// table). Only names+values the operator typed travel here; envelope-sealed
	// secrets never do.
	ExtraVars map[string]string `json:"extra_vars,omitempty"`
	// TolerateUnreachable opts an ansible run into the degraded outcome: a run
	// whose only non-success is unreachable hosts settles as a tolerated
	// degradation instead of a failure. It lives on the row so a daemon that
	// claims the job runs it under the same tolerance the operator asked for.
	TolerateUnreachable bool `json:"tolerate_unreachable,omitempty"`
	// Captures carries a bootstrap chain's env-decomposition wiring: the play
	// step's declared capture targets and the workflow id their sealed rows and
	// ab:<workflow> binding derive from. It is a first-class field precisely so a
	// step re-armed after a mid-run crash keeps it — rearmStep copies JobArgs, and
	// a play that lost its captures on resume would seal nothing and leave the
	// dependent apply's binding empty. It names files and env keys, never values.
	Captures *JobCaptures `json:"captures,omitempty"`
	// RunnerName names a registered Runner (internal/store Runner) whose
	// MemoryMax/CPUQuota the sandboxed execution path should enforce as a
	// per-job cgroup cap. Only the name travels here, mirroring Inventory and
	// RunSecretBinding: the runner row (and whether its caps still parse) is
	// resolved fresh at execution time, not cached onto the job. Empty is the
	// default, uncapped path most jobs take.
	RunnerName string `json:"runner_name,omitempty"`
	// Privileged records that this job was requested to run UNSANDBOXED (the
	// disabled tier) even when the daemon default is enforcing. It lives on the
	// row so whichever process claims the job runs it at the same tier the
	// requester asked for, and so the choice is an auditable job fact. Off by
	// default: an existing row without the field runs at the daemon's effective
	// tier exactly as before. Setting it is operator-gated at every face that
	// enqueues (see docs/security/threat-model.md).
	Privileged bool `json:"privileged,omitempty"`
}

// JobCaptures is a bootstrap chain's capture wiring on a job row. Workflow is the
// chain id both the play's sealer and the apply's ab:<workflow> binding derive
// from; Targets are the operator's declared capture points. No secret value ever
// lives here — only the file the play produced and the name it seals under.
type JobCaptures struct {
	Workflow string       `json:"workflow,omitempty"`
	Targets  []JobCapture `json:"targets,omitempty"`
}

// JobCapture is one declared capture: the output Name its sealed row and env
// binding derive from, the File (relative to the run's capture dir) the play
// fetched it into, and AsFile asking the consuming step to receive the sealed
// value as a path rather than an env value. It mirrors executor.AnsibleCapture,
// which the store package must not import (executor depends on store).
type JobCapture struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	AsFile bool   `json:"as_file,omitempty"`
}

// Duration is a time.Duration that marshals as a string ("30s") so job rows
// stay readable in plain SQLite.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Job is one row of the job table.
type Job struct {
	ID             int64
	Runbook        string
	Kind           string
	Trigger        string
	State          JobState
	Args           JobArgs
	QueuedAt       time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	ExitCode       *int
	ConcurrencyKey string
	DedupKey       string
	IdempotencyKey string
	OwnerPID       int
	// Partial is the legacy tolerated-degradation marker: an unreachable-tolerant
	// ansible run that reached every host it could and recorded the ones it could
	// not. The state column is now the truth (degraded); this flag is kept and
	// still written for readers that predate the degraded state.
	Partial bool
}

// NewJob describes a job to create. Every job starts queued.
type NewJob struct {
	Runbook        string
	Kind           string
	Trigger        string
	Args           JobArgs
	ConcurrencyKey string
	DedupKey       string
	IdempotencyKey string
}

const jobColumns = `id, runbook, kind, "trigger", state, args_json, queued_at,
	started_at, finished_at, exit_code, concurrency_key, dedup_key,
	idempotency_key, owner_pid, partial`

// CreateJob inserts a queued job. A duplicate idempotency_key returns
// ErrDuplicate; use JobByIdempotencyKey to recover the original.
func (s *Store) CreateJob(ctx context.Context, n NewJob) (Job, error) {
	args, err := json.Marshal(n.Args)
	if err != nil {
		return Job{}, fmt.Errorf("store: create job: encode args: %w", err)
	}
	now := time.Now().UTC()

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO job (runbook, kind, "trigger", state, args_json, queued_at,
		                 concurrency_key, dedup_key, idempotency_key, owner_pid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.Runbook, n.Kind, n.Trigger, string(StateQueued), string(args),
		now.UnixMilli(), n.ConcurrencyKey, nullStr(n.DedupKey),
		nullStr(n.IdempotencyKey), os.Getpid())
	if err != nil {
		return Job{}, wrap("create job", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Job{}, wrap("create job", err)
	}
	return s.GetJob(ctx, id)
}

// GetJob returns the job with the given id.
func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM job WHERE id = ?`, id)
	job, err := scanJob(row)
	if err != nil {
		return Job{}, wrap("get job", err)
	}
	return job, nil
}

// JobByIdempotencyKey returns the job created under key, or ErrNotFound.
func (s *Store) JobByIdempotencyKey(ctx context.Context, key string) (Job, error) {
	if key == "" {
		return Job{}, fmt.Errorf("store: job by idempotency key: %w", ErrNotFound)
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM job WHERE idempotency_key = ?`, key)
	job, err := scanJob(row)
	if err != nil {
		return Job{}, wrap("job by idempotency key", err)
	}
	return job, nil
}

// JobFilter narrows ListJobs. A zero filter lists every job, newest first.
type JobFilter struct {
	States         []JobState
	ConcurrencyKey string
	Runbook        string
	Limit          int
	// Offset skips this many matching rows before Limit applies, for a caller
	// paging past a window it already inspected (e.g. the queue's claim loop
	// paging past blocked rows rather than being capped by Limit alone).
	// Ignored when Limit is zero.
	Offset int
}

// ListJobs returns jobs matching the filter, oldest queued first.
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]Job, error) {
	query := `SELECT ` + jobColumns + ` FROM job`
	var where []string
	var args []any

	if len(f.States) > 0 {
		placeholders := make([]string, len(f.States))
		for i, st := range f.States {
			placeholders[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "state IN ("+strings.Join(placeholders, ", ")+")")
	}
	if f.ConcurrencyKey != "" {
		where = append(where, "concurrency_key = ?")
		args = append(args, f.ConcurrencyKey)
	}
	if f.Runbook != "" {
		where = append(where, "runbook = ?")
		args = append(args, f.Runbook)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY queued_at, id"
	if f.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, f.Limit, f.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list jobs", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, wrap("list jobs", err)
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list jobs", err)
	}
	return out, nil
}

// CountFailedJobsSince counts jobs that settled failed at or after since — the
// dashboard's "failed recently" card subline. A narrow COUNT rather than a
// ListJobs+len: the dashboard glance must never pull settled history into
// memory just to size a number.
func (s *Store) CountFailedJobsSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job WHERE state = ? AND finished_at >= ?`,
		string(StateFailed), since.UTC().UnixMilli()).Scan(&n)
	if err != nil {
		return 0, wrap("count failed jobs since", err)
	}
	return n, nil
}

// TransitionJob is the only way a job changes state. It rejects moves the
// state machine does not allow (ErrIllegalTransition) and moves whose source
// state no longer matches (ErrStateConflict). Entering running stamps
// started_at and the owning pid; entering a terminal state stamps finished_at.
func (s *Store) TransitionJob(ctx context.Context, id int64, from, to JobState) error {
	if !jobTransitions[from][to] {
		return fmt.Errorf("store: %s -> %s: %w", from, to, ErrIllegalTransition)
	}

	now := time.Now().UTC().UnixMilli()
	var (
		query string
		args  []any
	)
	switch {
	case to == StateRunning:
		query = `UPDATE job SET state = ?, started_at = COALESCE(started_at, ?), owner_pid = ?
		         WHERE id = ? AND state = ?`
		args = []any{string(to), now, os.Getpid(), id, string(from)}
	case to.Terminal():
		query = `UPDATE job SET state = ?, finished_at = ?, owner_pid = 0
		         WHERE id = ? AND state = ?`
		args = []any{string(to), now, id, string(from)}
	default:
		query = `UPDATE job SET state = ? WHERE id = ? AND state = ?`
		args = []any{string(to), id, string(from)}
	}

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return wrap("transition job", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("transition job", err)
	}
	if affected == 1 {
		return nil
	}

	job, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	return fmt.Errorf("store: job %d is %s, not %s: %w", id, job.State, from, ErrStateConflict)
}

// ClaimJob moves a queued job to running, refusing when a running row already
// holds the same concurrency key, or when the job still carries a pending
// approval. All three checks and the transition are one statement, so the
// database — not a process's memory — decides who runs: a daemon and a CLI over
// the same file cannot both find the key free, and neither can claim a job an
// operator has not yet approved, however that job reached queued.
//
// The pending-approval join is the claim-path half of the gate. A single gated
// apply is held by its awaiting_approval state; this makes a composed-chain step
// held the same durable way at the point of execution — even a queued row with
// an undecided approval is refused, so a mis-release or a race cannot run an
// unapproved step. A refusal that is not the job's state is ErrConcurrencyHeld
// or ErrApprovalPending, which a caller answers by trying another row.
func (s *Store) ClaimJob(ctx context.Context, id int64) error {
	now := time.Now().UTC().UnixMilli()
	res, err := s.db.ExecContext(ctx, `
		UPDATE job SET state = ?, started_at = COALESCE(started_at, ?), owner_pid = ?
		WHERE id = ? AND state = ?
		  AND NOT EXISTS (
		        SELECT 1 FROM approval
		        WHERE approval.job_id = job.id AND approval.state = ?)
		  AND (concurrency_key = '' OR NOT EXISTS (
		        SELECT 1 FROM job AS held
		        WHERE held.concurrency_key = job.concurrency_key
		          AND held.state = ? AND held.id <> job.id))`,
		string(StateRunning), now, os.Getpid(), id, string(StateQueued),
		string(ApprovalPending), string(StateRunning))
	if err != nil {
		return wrap("claim job", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("claim job", err)
	}
	if affected == 1 {
		return nil
	}

	job, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if job.State != StateQueued {
		return fmt.Errorf("store: job %d is %s, not %s: %w",
			id, job.State, StateQueued, ErrStateConflict)
	}
	// State is queued but the claim matched no row: either an unsettled approval
	// gates it, or a concurrency peer holds the key. The approval is the stronger
	// refusal and the one this guard exists for, so it is named first.
	pending, err := s.hasPendingApproval(ctx, id)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("store: claim job %d: approval unsettled: %w", id, ErrApprovalPending)
	}
	return fmt.Errorf("store: claim job %d: concurrency key %q is held: %w",
		id, job.ConcurrencyKey, ErrConcurrencyHeld)
}

// hasPendingApproval reports whether a job carries an approval still awaiting a
// decision, so a claim refusal can name the pending gate rather than a held key.
func (s *Store) hasPendingApproval(ctx context.Context, jobID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM approval WHERE job_id = ? AND state = ? LIMIT 1`,
		jobID, string(ApprovalPending)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, wrap("pending approval", err)
	}
	return true, nil
}

// SetJobExitCode records the subprocess exit code. Correctness is decided by
// this number alone; the state transition that follows reflects it.
func (s *Store) SetJobExitCode(ctx context.Context, id int64, code int) error {
	res, err := s.db.ExecContext(ctx, `UPDATE job SET exit_code = ? WHERE id = ?`, code, id)
	if err != nil {
		return wrap("set job exit code", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set job exit code", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set job exit code %d: %w", id, ErrNotFound)
	}
	return nil
}

// SetJobPartial marks a job as a tolerated degradation: a run whose only
// non-success was unreachable hosts. The degraded state is now the truth; this
// flag is the legacy marker kept and still written beside it (D2), so readers
// that predate the degraded state keep distinguishing a degraded run from a
// clean one.
func (s *Store) SetJobPartial(ctx context.Context, id int64, partial bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE job SET partial = ? WHERE id = ?`, boolInt(partial), id)
	if err != nil {
		return wrap("set job partial", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set job partial", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set job partial %d: %w", id, ErrNotFound)
	}
	return nil
}

// processStart is when this process loaded the package. A row owned by our own
// pid but stamped before this instant belongs to a previous process wearing
// the same number — the normal case in a container, where every restart is
// pid 1.
var processStart = time.Now().UTC()

// ownerAlive reports whether the process that owns a job row is still running.
// since is the row's own clock: started_at, or queued_at for a row that never
// started.
//
// Pid equality is not ownership. A container restart hands the new process the
// same pid 1 the dead one had, so a sweep keyed on "some other pid" leaves
// exactly the rows it exists to clean up.
func ownerAlive(pid int, since time.Time) bool {
	switch {
	case pid <= 0:
		// A terminal row, or one written before owner_pid meant anything.
		return false
	case pid == os.Getpid():
		return since.After(processStart)
	}
	// Signal 0 performs the permission and existence checks without sending
	// anything. EPERM means the process exists and is someone else's.
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Reconciliation summarises what one ReconcileNonTerminal pass repaired. Each
// count is a distinct class of stranded row the pass settled.
type Reconciliation struct {
	// Interrupted is queued/running rows whose owning process is gone.
	Interrupted int
	// Orphaned is awaiting_approval rows with no approval to decide them — the
	// crash window between a gated job's insert and its approval — retired to
	// interrupted once their creator is gone (finding #4).
	Orphaned int
	// Released is awaiting_approval rows whose approval settled approved but
	// whose release into the queue never landed (finding #3).
	Released int
	// Cancelled is awaiting_approval rows whose approval settled against them
	// (rejected, expired, withdrawn) but whose withdrawal never landed (#3).
	Cancelled int
}

// Total is the number of rows the pass moved.
func (r Reconciliation) Total() int {
	return r.Interrupted + r.Orphaned + r.Released + r.Cancelled
}

// ReconcileNonTerminal is the idle-time repair for every job the state machine
// still owes an answer for. It is one pass over queued, running, and
// awaiting_approval rows that moves each to what it should be, replacing the
// per-state crash-recovery sweeps:
//
//   - queued/running whose owning process is gone -> interrupted, terminal and
//     never auto-retried (the boot/idle interrupted-sweep this subsumes);
//   - awaiting_approval whose approval already settled but whose queue
//     transition never landed -> queued (approved) or cancelled
//     (rejected/expired/withdrawn), so a Decide interrupted after it recorded the
//     human's answer still finishes rather than stranding the job (finding #3);
//   - awaiting_approval with no approval row at all -> interrupted once its
//     creator is gone, so a gated job nobody can ever decide is not stuck
//     forever, yet a destructive apply is never auto-released (finding #4).
//
// A row whose gate is still pending is the gate working and is left alone; its
// TTL is the reactive layer's to run. "Gone" is decided by asking the kernel
// about the owning pid, not by pid equality (a container restart reissues pid 1).
// Every move is a guarded transition, so the pass is idempotent and safe to run
// on every idle tick: a row another process settles first is skipped, and a pass
// with nothing to do changes nothing.
func (s *Store) ReconcileNonTerminal(ctx context.Context) (Reconciliation, error) {
	rows, err := s.ListJobs(ctx, JobFilter{States: []JobState{
		StateQueued, StateRunning, StateAwaitingApproval}})
	if err != nil {
		return Reconciliation{}, err
	}

	var rec Reconciliation
	for _, job := range rows {
		switch job.State {
		case StateQueued, StateRunning:
			moved, err := s.reconcileOwned(ctx, job)
			if err != nil {
				return rec, err
			}
			if moved {
				rec.Interrupted++
			}
		case StateAwaitingApproval:
			kind, err := s.reconcileGated(ctx, job)
			if err != nil {
				return rec, err
			}
			switch kind {
			case reconcileOrphaned:
				rec.Orphaned++
			case reconcileReleased:
				rec.Released++
			case reconcileCancelled:
				rec.Cancelled++
			}
		}
	}
	return rec, nil
}

// reconcileKind is what reconcileGated did to one awaiting_approval row.
type reconcileKind int

const (
	reconcileNone reconcileKind = iota
	reconcileOrphaned
	reconcileReleased
	reconcileCancelled
)

// reconcileOwned retires a queued or running job to interrupted when its owning
// process is gone, the crash-recovery the boot and idle sweeps have always run.
//
// A queued row's owner_pid is whoever last wrote the row (CreateJob,
// CreateGatedJob, or Release, none of which touch it) — for the many ephemeral
// CLI paths (queue.New(st, enqueueOnly{}, ...) in run.go, provision.go,
// handoff.go, bootstrap.go, adhoc.go, playbook.go, workspace.go, backup.go,
// vendor.go) that is the short-lived CLI, not the daemon that will eventually
// ClaimJob it — and ClaimJob itself never checks owner_pid, so a queued row is
// claimable by any worker regardless of who enqueued it. This looks like it
// could retire a genuinely still-runnable row purely because its enqueuer
// exited first. Verified: it is reachable (a CLI killed after Release, e.g.
// Ctrl-C while its job sits queued behind a held concurrency key — the exact
// case shutdownServe's own re-sweep handles), but it is not a hazard:
//
//   - a workflow step (queue.Sequencer) is self-healing: ResumeAll (driven
//     every second by the daemon, see cmd/exocomp/serve.go's driveWorkflows)
//     sees the ledger step still unfinished, and rearmStep re-enqueues it
//     under a FRESH job whose CreateGatedJob stamps the calling (long-lived
//     daemon) pid, which ownerAlive treats as always alive. No work is lost;
//     see TestSequencerRearmsStepInterruptedWhileQueued.
//   - a standalone job (no ledger) has no such backstop, but this is the
//     deliberate contract, not an oversight: interrupted is terminal and never
//     auto-retried (see ListInterruptedJobs), so the operator whose own CLI
//     died before the daemon could claim the row gets an explicit, visible
//     "this needs re-driving" rather than the daemon silently running a
//     request nobody is left tracking. TestReconcilePreservesInterruptedSweep
//     pins exactly this for a queued row.
func (s *Store) reconcileOwned(ctx context.Context, job Job) (bool, error) {
	if ownerAlive(job.OwnerPID, jobClock(job)) {
		return false, nil
	}
	return s.reconcileTransition(ctx, job.ID, job.State, StateInterrupted)
}

// reconcileGated settles one awaiting_approval row against its approval. A row
// whose approval is still pending is the gate doing its job and is left alone; a
// row whose approval already settled has its lost queue transition finished; a
// row with no approval at all can be decided by no one and is retired to
// interrupted once its creator is gone. It never releases a row without a
// settled approval — a destructive apply is not run without a human's answer.
func (s *Store) reconcileGated(ctx context.Context, job Job) (reconcileKind, error) {
	approval, err := s.ApprovalByJob(ctx, job.ID)
	if errors.Is(err, ErrNotFound) {
		// A live creator may be between the gated insert and CreateApproval;
		// racing it would strand a job it is about to gate, so wait for it to die.
		if ownerAlive(job.OwnerPID, jobClock(job)) {
			return reconcileNone, nil
		}
		moved, err := s.reconcileTransition(ctx, job.ID, StateAwaitingApproval, StateInterrupted)
		if err != nil || !moved {
			return reconcileNone, err
		}
		return reconcileOrphaned, nil
	}
	if err != nil {
		return reconcileNone, err
	}
	if approval.State == ApprovalPending {
		return reconcileNone, nil
	}

	to, kind := StateCancelled, reconcileCancelled
	if approval.State == ApprovalApproved {
		to, kind = StateQueued, reconcileReleased
	}
	moved, err := s.reconcileTransition(ctx, job.ID, StateAwaitingApproval, to)
	if err != nil || !moved {
		return reconcileNone, err
	}
	return kind, nil
}

// reconcileTransition applies one repair move through the same guarded
// TransitionJob the live paths use, so the released/withdrawn/interrupted stamps
// are identical to a settle that was not interrupted. A row another process
// already moved (ErrStateConflict) is success, not a failure: the reconcile
// races the real Decide path, and a healthy concurrent settle is exactly what it
// exists to allow.
func (s *Store) reconcileTransition(ctx context.Context, id int64, from, to JobState) (bool, error) {
	err := s.TransitionJob(ctx, id, from, to)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrStateConflict) {
		return false, nil
	}
	return false, err
}

// jobClock is the row's own clock for a liveness test: started_at, or queued_at
// for a row that never started.
func jobClock(job Job) time.Time {
	if job.StartedAt != nil {
		return *job.StartedAt
	}
	return job.QueuedAt
}

// ListInterruptedJobs returns jobs retired to interrupted, most recent first.
// Interrupted is terminal and never auto-retried (ReconcileNonTerminal), so this is
// the post-reboot view an operator re-drives from: the runs a shutdown or a
// crash cut off, newest at the top. limit <= 0 returns them all.
func (s *Store) ListInterruptedJobs(ctx context.Context, limit int) ([]Job, error) {
	query := `SELECT ` + jobColumns + ` FROM job WHERE state = ?
		ORDER BY finished_at DESC, id DESC`
	args := []any{string(StateInterrupted)}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list interrupted jobs", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, wrap("list interrupted jobs", err)
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list interrupted jobs", err)
	}
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var (
		job                        Job
		state, argsJSON            string
		queuedAt                   int64
		startedAt, finishedAt      sql.NullInt64
		exitCode                   sql.NullInt64
		dedupKey, idempotencyKey   sql.NullString
		ownerPID                   int64
		partial                    int64
		runbook, kind, triggerName string
	)
	err := row.Scan(&job.ID, &runbook, &kind, &triggerName, &state, &argsJSON,
		&queuedAt, &startedAt, &finishedAt, &exitCode, &job.ConcurrencyKey,
		&dedupKey, &idempotencyKey, &ownerPID, &partial)
	if err != nil {
		return Job{}, err
	}

	job.Runbook = runbook
	job.Kind = kind
	job.Trigger = triggerName
	job.State = JobState(state)
	job.QueuedAt = time.UnixMilli(queuedAt).UTC()
	job.StartedAt = timePtr(startedAt)
	job.FinishedAt = timePtr(finishedAt)
	job.ExitCode = intPtr(exitCode)
	job.OwnerPID = int(ownerPID)
	job.Partial = partial != 0
	if dedupKey.Valid {
		job.DedupKey = dedupKey.String
	}
	if idempotencyKey.Valid {
		job.IdempotencyKey = idempotencyKey.String
	}
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &job.Args); err != nil {
			return Job{}, fmt.Errorf("decode args of job %d: %w", job.ID, err)
		}
	}
	return job, nil
}
