package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// Every write tool routes through the SAME internal/service verb the CLI and the
// HTTP write API call (invariant 5): the verb assembles the queue.Spec and makes
// the gate decision, and this face supplies a pure-enqueue Enqueuer. The local
// view kinds below carry identifiers and states only, never values — the wire
// package names no write projection, so, like internal/web, they live beside the
// face that emits them.
const (
	kindJobEnqueued     = "job.enqueued"
	kindApprovalPending = "approval.pending"
	kindDecision        = "approval.decision"
)

type jobEnqueuedView struct {
	JobID int64  `json:"job_id"`
	State string `json:"state,omitempty"`
}

// approvalPendingView is a gated apply: the job sits in awaiting_approval and
// this approval releases it. ttl_seconds and expires_at let an agent read the
// fail-closed rejection at TTL as intended, not as an error (agent-interface §4).
type approvalPendingView struct {
	ApprovalID     int64  `json:"approval_id"`
	JobID          int64  `json:"job_id"`
	State          string `json:"state"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	TTLSeconds     int64  `json:"ttl_seconds,omitempty"`
	Policy         string `json:"policy,omitempty"`
	PlanArtifactID int64  `json:"plan_artifact_id,omitempty"`
}

type decisionView struct {
	ApprovalID int64  `json:"approval_id"`
	Outcome    string `json:"outcome,omitempty"`
	Message    string `json:"message,omitempty"`
}

// errPlanDecryptUnsupported is the buildArgv refusal for a plain saved-plan
// apply: decrypting the sealed plan to a run-private plaintext is CLI-resident,
// so the verb is refused rather than enqueuing an apply whose argv the daemon
// cannot read. It matches the HTTP write API's identical deferral.
var errPlanDecryptUnsupported = errors.New(
	"mcp: applying a saved plan without a policy verdict needs the plan-decrypt helper (not yet shared)")

// planWriteArgs is the shared argument shape for plan and apply. All optional; a
// bare {name} is the common case.
type planWriteArgs struct {
	Name           string `json:"name"`
	RunSecrets     string `json:"run_secrets"`
	Timeout        string `json:"timeout"`
	IdempotencyKey string `json:"idempotency_key"`
	// Plan names a saved plan artifact by id for apply; empty is a fresh apply.
	Plan string `json:"plan"`
}

func parseTimeout(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	return time.ParseDuration(v)
}

// tofuArtifactsDir is where a plan's -out file lands, mirroring the CLI and the
// HTTP API. It sits inside the 0700 data dir: plan files hold resource values in
// the clear until they are sealed.
func (s *Server) tofuArtifactsDir() (string, error) {
	dir := filepath.Join(s.dataDir, "artifacts", "tofu")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// toolWorkspacePlan enqueues a tofu plan through PlanWorkspace and returns the
// job id. Sealing the plan into a consumable artifact is a CLI-resident post-run
// step (recordPlan/evalPlanPolicy), not done here — the same boundary the HTTP
// write API draws.
func (s *Server) toolWorkspacePlan(ctx context.Context, args planWriteArgs) (string, any, error) {
	if args.Name == "" {
		return "", nil, invalidArg("workspace name is required")
	}
	timeout, err := parseTimeout(args.Timeout)
	if err != nil {
		return "", nil, invalidArg("timeout is not a valid Go duration")
	}
	ws, err := s.store.WorkspaceByName(ctx, args.Name)
	if err != nil {
		return "", nil, err
	}
	artifacts, err := s.tofuArtifactsDir()
	if err != nil {
		return "", nil, err
	}
	res, err := s.service().PlanWorkspace(ctx, s.newEnqueuer(), service.PlanInput{
		Workspace:      ws.Name,
		TofuDir:        ws.TofuDir,
		ArtifactsDir:   artifacts,
		RunSecrets:     args.RunSecrets,
		IdempotencyKey: args.IdempotencyKey,
		Timeout:        timeout,
	})
	if err != nil {
		return "", nil, err
	}
	return kindJobEnqueued, jobEnqueuedView{JobID: res.Job.ID, State: string(res.Job.State)}, nil
}

// toolWorkspaceApply enqueues a tofu apply through ApplyWorkspace. A fresh apply
// (no plan) auto-approves and enqueues plainly. A saved plan named by artifact
// id is routed by its recorded policy verdict: deny is refused, require_approval
// lands the job in awaiting_approval behind a fresh approval (the pending view),
// and a plain saved-plan apply is refused for now (decrypt is CLI-resident).
func (s *Server) toolWorkspaceApply(ctx context.Context, args planWriteArgs) (string, any, error) {
	if args.Name == "" {
		return "", nil, invalidArg("workspace name is required")
	}
	timeout, err := parseTimeout(args.Timeout)
	if err != nil {
		return "", nil, invalidArg("timeout is not a valid Go duration")
	}
	ws, err := s.store.WorkspaceByName(ctx, args.Name)
	if err != nil {
		return "", nil, err
	}

	var saved *store.Artifact
	var gate service.PlanGate
	if args.Plan != "" {
		id, perr := strconv.ParseInt(args.Plan, 10, 64)
		if perr != nil || id <= 0 {
			return "", nil, invalidArg("plan must be a saved plan artifact id")
		}
		art, gerr := s.store.GetArtifact(ctx, id)
		if gerr != nil {
			return "", nil, gerr
		}
		if art.Kind != reactive.PlanArtifactKind {
			return "", nil, invalidArg("artifact " + args.Plan + " is not a plan")
		}
		saved = &art
		if gate, err = s.planGate(ctx, art); err != nil {
			return "", nil, err
		}
	}

	deriver, closeEnv, err := s.approvalDeriver(saved, gate)
	if err != nil {
		return "", nil, err
	}
	if closeEnv != nil {
		defer closeEnv()
	}

	res, err := s.service().ApplyWorkspace(ctx, s.newEnqueuer(), deriver, service.ApplyInput{
		Workspace:       ws.Name,
		WorkspaceSerial: ws.Serial,
		TofuDir:         ws.TofuDir,
		RunSecrets:      args.RunSecrets,
		IdempotencyKey:  args.IdempotencyKey,
		Timeout:         timeout,
		SavedPlan:       saved,
		Gate:            gate,
	}, func(context.Context) ([]string, *store.Artifact, func(), error) {
		if saved == nil {
			return executor.TofuArgv("apply", "-auto-approve"), nil, func() {}, nil
		}
		return nil, nil, func() {}, errPlanDecryptUnsupported
	})
	if err != nil {
		return "", nil, err
	}

	if res.Gated {
		a := res.Approval
		return kindApprovalPending, approvalPendingView{
			ApprovalID:     a.ApprovalID,
			JobID:          a.JobID,
			State:          string(a.State),
			ExpiresAt:      a.ExpiresAt.UTC().Format(time.RFC3339),
			TTLSeconds:     int64(a.ExpiresAt.Sub(s.now()).Seconds()),
			Policy:         a.Policy,
			PlanArtifactID: a.PlanArtifactID,
		}, nil
	}

	run := res.Run
	defer run.Release()
	return kindJobEnqueued, jobEnqueuedView{JobID: run.Job.ID, State: string(run.Job.State)}, nil
}

// approvalDeriver opens the secrets envelope only when the apply will actually
// route through the gate (a require-approval saved plan), because that is the
// only path that mints an approval capability. A fresh apply and a deny/plain
// plan need no deriver, so the envelope — and the keyring it needs — is never
// touched for them.
func (s *Server) approvalDeriver(saved *store.Artifact, gate service.PlanGate) (reactive.Deriver, func(), error) {
	if saved == nil || !gate.RequireApproval {
		return nil, nil, nil
	}
	return s.openEnvelope()
}

// planGate reads back the policy verdict a plan's seal recorded on the plan
// job's timeline, reduced to the two outcomes that route an apply. It reads only
// the values-free plan_policy audit envelope (name + action + artifact id),
// never a plan attribute value — the same store read the HTTP write API's
// planGate does.
func (s *Server) planGate(ctx context.Context, art store.Artifact) (service.PlanGate, error) {
	if art.JobID == 0 {
		return service.PlanGate{}, nil
	}
	events, err := s.store.ListJobEvents(ctx, art.JobID)
	if err != nil {
		return service.PlanGate{}, err
	}
	for _, e := range events {
		if e.RawJSON == "" {
			continue
		}
		var env struct {
			PlanPolicy *struct {
				Action   string `json:"action"`
				Policy   string `json:"policy"`
				Artifact int64  `json:"artifact"`
			} `json:"plan_policy"`
		}
		if err := json.Unmarshal([]byte(e.RawJSON), &env); err != nil || env.PlanPolicy == nil {
			continue
		}
		if env.PlanPolicy.Artifact != art.ID {
			continue
		}
		return service.PlanGate{
			Deny:            env.PlanPolicy.Action == store.PolicyDeny,
			RequireApproval: env.PlanPolicy.Action == store.PolicyRequireApproval,
			Policy:          env.PlanPolicy.Policy,
		}, nil
	}
	return service.PlanGate{}, nil
}

// decideArgs is the argument shape of approve_job / reject_job: an approval id.
type decideArgs struct {
	ApprovalID int64 `json:"approval_id"`
}

// decide resolves one approval through the reactive engine — the single
// fail-closed place a human's answer enters the system (agent-interface §3.5).
// The decision capability is derived SERVER-SIDE from the approval's job, never
// carried by the caller and never returned: a scoped tool call is not a decision
// link, so a low-authority agent cannot be silently promoted to approver. The
// engine's queue is the pure-enqueue queue — an approved job is released to
// queued and the daemon runs it.
func (s *Server) decide(ctx context.Context, decision string, in decideArgs) (string, any, error) {
	if in.ApprovalID <= 0 {
		return "", nil, invalidArg("approval_id must be a positive integer")
	}
	approval, err := s.store.GetApproval(ctx, in.ApprovalID)
	if err != nil {
		return "", nil, err
	}

	deriver, closeEnv, err := s.openEnvelope()
	if err != nil {
		return "", nil, err
	}
	if closeEnv != nil {
		defer closeEnv()
	}

	q := queue.New(s.store, noRunDriver{}, queue.Options{Workers: 1})
	eng, err := reactive.New(reactive.Options{Store: s.store, Queue: q, Deriver: deriver})
	if err != nil {
		return "", nil, err
	}

	token, _, err := reactive.ApprovalToken(deriver, approval.JobID)
	if err != nil {
		return "", nil, err
	}
	res, err := s.service().DecideApproval(ctx, eng, service.DecideInput{
		ApprovalID: in.ApprovalID,
		Decision:   decision,
		Token:      token,
		Via:        "mcp",
	})
	if err != nil {
		return "", nil, err
	}
	return kindDecision, decisionView{
		ApprovalID: in.ApprovalID,
		Outcome:    res.Decision.Outcome,
		Message:    res.Decision.Message,
	}, nil
}
