package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"exocomp.dev/exocomp/internal/executor"
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

// Tool labels for the idempotency namespace, and the namespacing itself. A
// caller-chosen key is scoped to this face and the tool that used it, so an
// agent's guessable key ("deploy-<date>", a ticket id) cannot collide with
// another caller's write or address the queue's reserved `wf:` workflow step
// keys (finding AI-7). Stdio has no principal to scope by; when it grows a
// token, the token id joins this prefix.
const (
	toolPlanKey  = "workspace.plan"
	toolApplyKey = "workspace.apply"
)

func idempotencyKey(tool, key string) string {
	if key == "" {
		return ""
	}
	return "mcp:" + tool + ":" + key
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
		IdempotencyKey: idempotencyKey(toolPlanKey, args.IdempotencyKey),
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
		// Fail-closed adjudication: a plan on a policied workspace with no
		// recorded verdict is unconsumable, not unpoliced (finding AI-3).
		if gate, err = s.service().PlanGateForPlan(ctx, ws.Name, art); err != nil {
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
		IdempotencyKey:  idempotencyKey(toolApplyKey, args.IdempotencyKey),
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

// There is deliberately no decide seam here. The stdio transport authenticates
// nothing, so an agent holding workspace_apply would also hold the release for
// the gate that apply raises (finding AI-1). An approval is decided from the
// browser/phone surface, the CLI, or the scope-checked /api/v1 decision route.
