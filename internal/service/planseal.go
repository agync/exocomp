package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/policy"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// PlanEnvelope is the slice of the secrets envelope the plan seal/open seam
// needs: it seals a plan file to the keyring's recipients and opens a sealed one
// for the one run that consumes it. The secrets envelope satisfies it; an
// interface keeps the seam testable without a real keyring.
type PlanEnvelope interface {
	SealFile(path string) (string, error)
	OpenSealedFile(path string) ([]byte, error)
}

// SealPlan registers the plan file a run wrote, encrypted at rest first. A run
// that changed nothing still writes one, so its absence means the plan never got
// that far, and SealPlan returns a nil artifact. The digest recorded is of the
// ciphertext, because that is what is on disk to check later. A nil envelope
// records the plaintext path unchanged (there is nothing to seal to).
func (s *Service) SealPlan(ctx context.Context, env PlanEnvelope, jobID int64, path string) (*store.Artifact, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if env != nil {
		sealed, err := env.SealFile(path)
		if err != nil {
			return nil, fmt.Errorf("workspace: seal %s artifact: %w", reactive.PlanArtifactKind, err)
		}
		path = sealed
	}
	sum, size, err := fileDigest(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	saved, err := s.store.CreateArtifact(ctx, store.Artifact{
		JobID:  jobID,
		Kind:   reactive.PlanArtifactKind,
		Path:   path,
		SHA256: sum,
		Size:   size,
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

// OpenPlanForRun writes a saved plan out in the clear for the one run that
// consumes it, under the 0700 private root, and returns the path with the
// function that removes it. tofu takes a file, not a stream, so the plaintext
// has to exist — for the length of the apply and no longer. A plan that is not
// sealed (an operator's own plan file), or a nil envelope, is passed through
// untouched.
func OpenPlanForRun(env PlanEnvelope, privateRoot, path string) (string, func(), error) {
	noop := func() {}
	if env == nil || filepath.Ext(path) != secrets.SealedSuffix {
		return path, noop, nil
	}

	plaintext, err := env.OpenSealedFile(path)
	if err != nil {
		return "", noop, fmt.Errorf("workspace apply: %w", err)
	}
	dir, err := os.MkdirTemp(privateRoot, "plan-")
	if err != nil {
		return "", noop, fmt.Errorf("workspace apply: private dir: %w", err)
	}
	out := filepath.Join(dir, "saved.tfplan")
	if err := os.WriteFile(out, plaintext, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", noop, fmt.Errorf("workspace apply: write plan: %w", err)
	}
	clear(plaintext)
	return out, func() { _ = os.RemoveAll(dir) }, nil
}

// planPolicyVerdict is the audit record a matched policy writes onto the plan
// job's timeline, keyed to the sealed artifact it decided. It carries only the
// policy name and action — never any of the plan's attribute values.
type planPolicyVerdict struct {
	Action   string `json:"action"`
	Policy   string `json:"policy"`
	Artifact int64  `json:"artifact"`
}

// PlanPolicyResult is the verdict a plan's policies produced. FailNotice is set
// when evaluation could not complete and the plan was failed closed (sealed
// denied); a face renders it and the verdict is a deny. Evaluated reports that
// the workspace HAD enabled policies and they ran — a caller must record the
// outcome either way (RecordPlanVerdict on a match, RecordPlanAdjudication on
// none), because an apply reads the absence of any record as "not adjudicated
// yet" and refuses (finding AI-3).
type PlanPolicyResult struct {
	Verdict    policy.Verdict
	FailNotice string
	Evaluated  bool
}

var (
	errNoPlanOutcome = errors.New("the plan recorded no outcome")
	errNoPlanDetail  = errors.New("the plan reported changes without resource detail")
)

// EvalPlanPolicy evaluates a workspace's enabled policies against a sealed
// plan's outcome and returns the governing verdict. A workspace with no policies
// pays nothing. The plan is read from the outcome the tofu driver already parsed
// out of `tofu show -json` and recorded on the plan job — a values-free view of
// what the plan changes, so a policy decides on structure and no plan secret is
// ever in play. A plan that changed something the driver could not describe is
// failed closed: a plan a policy could not examine must not apply unseen.
func (s *Service) EvalPlanPolicy(ctx context.Context, ws store.Workspace, jobID int64) (PlanPolicyResult, error) {
	policies, err := s.store.ListPlanPolicies(ctx, ws.Name, true)
	if err != nil {
		return PlanPolicyResult{}, err
	}
	if len(policies) == 0 {
		return PlanPolicyResult{}, nil
	}

	outcome, ok, err := s.planOutcome(ctx, jobID)
	if err != nil {
		return PlanPolicyResult{}, err
	}
	if !ok {
		return failClosed("read the plan outcome", errNoPlanOutcome), nil
	}
	// A plan with changes but no described detail is a plan the driver's own
	// `show` could not render — the one case that must not slip through, since a
	// delete rule sees an empty list and would clear it.
	if outcome.Changes && len(outcome.Pending) == 0 && len(outcome.Drift) == 0 {
		return failClosed("describe the plan's changes", errNoPlanDetail), nil
	}

	plan := policy.Plan(outcome.Changes,
		toPlanChanges(outcome.Pending), toPlanChanges(outcome.Drift))
	verdict, err := policy.Evaluate(ctx, policies, plan)
	if err != nil {
		return failClosed("evaluate policies", err), nil
	}
	return PlanPolicyResult{Verdict: verdict, Evaluated: true}, nil
}

// failClosed turns an evaluation that could not complete into a deny, so a plan
// no policy could decide is sealed unconsumable rather than waved through.
func failClosed(stage string, err error) PlanPolicyResult {
	return PlanPolicyResult{
		Verdict:    policy.Verdict{Policy: "policy-evaluation-failed", Action: store.PolicyDeny},
		FailNotice: fmt.Sprintf("plan policy: could not %s (%v); sealing the plan denied", stage, err),
		Evaluated:  true,
	}
}

// planOutcome reads the structured plan outcome the tofu driver recorded on the
// plan job — the resource-level drift and pending changes it parsed from
// `tofu show -json`, with values already suppressed.
func (s *Service) planOutcome(ctx context.Context, jobID int64) (executor.TofuPlanOutcome, bool, error) {
	events, err := s.store.ListJobEvents(ctx, jobID)
	if err != nil {
		return executor.TofuPlanOutcome{}, false, err
	}
	for _, e := range events {
		if e.RawJSON == "" {
			continue
		}
		var envelope struct {
			Tofu *executor.TofuPlanOutcome `json:"tofu"`
		}
		if err := json.Unmarshal([]byte(e.RawJSON), &envelope); err != nil || envelope.Tofu == nil {
			continue
		}
		return *envelope.Tofu, true, nil
	}
	return executor.TofuPlanOutcome{}, false, nil
}

// toPlanChanges maps the driver's resource deltas onto the values-free view a
// policy binds to. The two types carry the same fields; the copy keeps the
// policy package free of an executor dependency.
func toPlanChanges(deltas []executor.ResourceDelta) []policy.PlanChange {
	out := make([]policy.PlanChange, len(deltas))
	for i, d := range deltas {
		out[i] = policy.PlanChange{Address: d.Address, Actions: d.Actions, AttrKeys: d.AttrKeys}
	}
	return out
}

// RecordPlanVerdict writes a matched policy's decision onto the plan job's
// timeline, keyed to the sealed artifact. The record is the audit trail and the
// apply-time gate reads it back; it names the policy and action only, so a plan
// secret never reaches an event. It returns the one-line notice a face renders.
func (s *Service) RecordPlanVerdict(ctx context.Context, jobID, artifactID int64, v policy.Verdict) (string, error) {
	payload, err := json.Marshal(map[string]planPolicyVerdict{
		"plan_policy": {Action: v.Action, Policy: v.Policy, Artifact: artifactID},
	})
	if err != nil {
		return "", fmt.Errorf("workspace: encode plan verdict: %w", err)
	}
	msg := fmt.Sprintf("plan policy: %s by %q", v.Action, v.Policy)
	if _, err := s.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: jobID, Level: store.LevelSystem, Msg: msg, RawJSON: string(payload),
	}); err != nil {
		return "", err
	}
	return msg, nil
}

// planActionNoMatch is the action of the adjudication record written when a
// workspace's policies ran against a plan and none matched. It is deliberately
// not a store.Policy* action: nothing routes on it, and PlanVerdict skips it, so
// every existing consumer sees the same zero verdict it always did. Its only
// reader is planAdjudicated, which needs to tell "policies ran, nothing matched"
// apart from "the verdict has not been written yet" (finding AI-3).
const planActionNoMatch = "no_match"

// RecordPlanAdjudication records that a workspace's enabled policies were
// evaluated against a sealed plan and none of them matched. Without it, a
// legitimately-unmatched plan would be indistinguishable from a plan whose
// verdict has not landed yet, and the fail-closed apply gate would refuse it.
func (s *Service) RecordPlanAdjudication(ctx context.Context, jobID, artifactID int64) error {
	payload, err := json.Marshal(map[string]planPolicyVerdict{
		"plan_policy": {Action: planActionNoMatch, Artifact: artifactID},
	})
	if err != nil {
		return fmt.Errorf("workspace: encode plan adjudication: %w", err)
	}
	if _, err := s.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: jobID, Level: store.LevelSystem,
		Msg:     "plan policy: evaluated, no policy matched",
		RawJSON: string(payload),
	}); err != nil {
		return err
	}
	return nil
}

// PlanVerdict reads back the policy verdict recorded on a sealed plan, or a zero
// verdict when none was recorded (no policies, or a plan applied by a path this
// control plane never sealed). It is what makes a deny plan unconsumable: the
// apply consults it before it enqueues anything.
func (s *Service) PlanVerdict(ctx context.Context, artifact store.Artifact) (policy.Verdict, error) {
	rec, ok, err := s.planPolicyRecord(ctx, artifact)
	if err != nil || !ok || rec.Action == planActionNoMatch {
		return policy.Verdict{}, err
	}
	return policy.Verdict{Policy: rec.Policy, Action: rec.Action}, nil
}

// planPolicyRecord finds the plan_policy record keyed to one artifact, whether
// it names a matched verdict or the no-match adjudication.
func (s *Service) planPolicyRecord(ctx context.Context, artifact store.Artifact) (planPolicyVerdict, bool, error) {
	if artifact.JobID == 0 {
		return planPolicyVerdict{}, false, nil
	}
	events, err := s.store.ListJobEvents(ctx, artifact.JobID)
	if err != nil {
		return planPolicyVerdict{}, false, err
	}
	for _, e := range events {
		if e.RawJSON == "" {
			continue
		}
		var envelope struct {
			PlanPolicy *planPolicyVerdict `json:"plan_policy"`
		}
		if err := json.Unmarshal([]byte(e.RawJSON), &envelope); err != nil || envelope.PlanPolicy == nil {
			continue
		}
		if envelope.PlanPolicy.Artifact != artifact.ID {
			continue
		}
		return *envelope.PlanPolicy, true, nil
	}
	return planPolicyVerdict{}, false, nil
}

// PlanUnadjudicatedError reports a saved plan that a workspace's enabled
// policies have not decided: the seal wrote the artifact row but no verdict
// record is on the plan job's timeline yet, or the plan was never adjudicated at
// all (an operator's own plan file). Applying it would consult a policy set that
// never saw it, so it is refused. It unwraps to the store's state-conflict
// sentinel, so every face renders it as a conflict without its own mapping.
type PlanUnadjudicatedError struct {
	PlanArtifactID int64
	Workspace      string
}

func (e *PlanUnadjudicatedError) Error() string {
	return fmt.Sprintf("workspace apply: plan %d is unconsumable: workspace %q has enabled plan "+
		"policies and this plan carries no recorded verdict; re-plan and apply that plan by id",
		e.PlanArtifactID, e.Workspace)
}

func (e *PlanUnadjudicatedError) Unwrap() error { return store.ErrStateConflict }

// PlanWorkspaceMismatchError reports a saved plan being applied in a workspace
// that is not the one that produced it. The plan's owner is read from its plan
// job's runbook, the only place a queued tofu job records its workspace.
type PlanWorkspaceMismatchError struct {
	PlanArtifactID int64
	Owner          string
	Asked          string
}

func (e *PlanWorkspaceMismatchError) Error() string {
	return fmt.Sprintf("workspace apply: plan %d belongs to workspace %q, not %q",
		e.PlanArtifactID, e.Owner, e.Asked)
}

// PlanOwner returns the workspace a saved plan was produced for, read back from
// its plan job's runbook. An artifact with no job — an operator's own plan file,
// resolved by path — has no recorded owner and returns "".
func (s *Service) PlanOwner(ctx context.Context, art store.Artifact) (string, error) {
	if art.JobID == 0 {
		return "", nil
	}
	job, err := s.store.GetJob(ctx, art.JobID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	name, ok := strings.CutPrefix(job.Runbook, workspaceRunbookPrefix)
	if !ok {
		return "", nil
	}
	return name, nil
}

// PlanGateForPlan resolves the gate a saved plan's recorded verdict implies,
// refusing a plan produced for another workspace, and otherwise
// FAIL-CLOSED: on a workspace carrying enabled plan policies, a plan with no
// adjudication record is refused rather than treated as unpoliced. That closes
// the window between CreateArtifact and the verdict event, where an apply-by-id
// issued against a freshly-sealed plan used to bypass deny/require_approval
// entirely (finding AI-3). It is the one adjudication read every face uses.
func (s *Service) PlanGateForPlan(ctx context.Context, workspace string, art store.Artifact) (PlanGate, error) {
	// A plan is adjudicated for the workspace that produced it and no other:
	// applying one workspace's sealed plan inside another runs it under the
	// wrong backend bearer and state passphrase, and reads the stale-plan serial
	// from a workspace the plan was never made against (finding AI-9).
	owner, err := s.PlanOwner(ctx, art)
	if err != nil {
		return PlanGate{}, err
	}
	if owner != "" && owner != workspace {
		return PlanGate{}, &PlanWorkspaceMismatchError{PlanArtifactID: art.ID, Owner: owner, Asked: workspace}
	}
	rec, recorded, err := s.planPolicyRecord(ctx, art)
	if err != nil {
		return PlanGate{}, err
	}
	if !recorded {
		policies, perr := s.store.ListPlanPolicies(ctx, workspace, true)
		if perr != nil {
			return PlanGate{}, perr
		}
		if len(policies) > 0 {
			return PlanGate{}, &PlanUnadjudicatedError{PlanArtifactID: art.ID, Workspace: workspace}
		}
		return PlanGate{}, nil
	}
	if rec.Action == planActionNoMatch {
		return PlanGate{}, nil
	}
	return PlanGateFor(policy.Verdict{Policy: rec.Policy, Action: rec.Action}), nil
}

// PlanGateFor reduces a plan's recorded verdict to the two outcomes that route
// an apply: deny makes the plan unconsumable, and require-approval sends it
// through the gate.
func PlanGateFor(v policy.Verdict) PlanGate {
	return PlanGate{Deny: v.Deny(), RequireApproval: v.RequireApproval(), Policy: v.Policy}
}

// fileDigest is the sha256 and byte size of a file on disk.
func fileDigest(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	size, err = io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("digest %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
