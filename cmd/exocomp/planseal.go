package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/policy"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// sensitiveArtifact reports whether an artifact of this kind is encrypted at
// rest. A saved OpenTofu plan is: it carries every attribute of every resource
// it would change, in the clear, which the approval view renders and the
// database never stores. The list is a policy, not a schema — the kind on the
// row decides, so an artifact that outlives its row is still readable by name.
func sensitiveArtifact(kind string) bool {
	return kind == planArtifactKind
}

// sealArtifact encrypts a sensitive artifact to the keyring's recipients and
// returns the path it now lives at. A kind that is not sensitive keeps its
// path.
func sealArtifact(env *secrets.Envelope, kind, path string) (string, error) {
	if env == nil || !sensitiveArtifact(kind) {
		return path, nil
	}
	sealed, err := env.SealFile(path)
	if err != nil {
		return "", fmt.Errorf("workspace: seal %s artifact: %w", kind, err)
	}
	return sealed, nil
}

// openPlanForRun writes a saved plan out in the clear for the one run that
// consumes it, under the 0700 private dir, and returns the path with the
// function that removes it. tofu takes a file, not a stream, so the plaintext
// has to exist — for the length of the apply and no longer.
func openPlanForRun(env *secrets.Envelope, privateRoot, path string) (string, func(), error) {
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

// The plan-policy seam. A plan policy is a CEL rule over the `tofu show -json`
// document; it is evaluated when a plan is sealed and its verdict recorded on
// the plan's own job, so a later apply of that sealed plan can read the verdict
// back. The verdict carries only the policy name and action — never any of the
// plan's attribute values — so it is safe to persist and redaction-clean.

// planPolicyVerdict is the audit record a matched policy writes onto the plan
// job's timeline, keyed to the sealed artifact it decided.
type planPolicyVerdict struct {
	Action   string `json:"action"`
	Policy   string `json:"policy"`
	Artifact int64  `json:"artifact"`
}

// evalPlanPolicy evaluates a workspace's enabled policies against a sealed
// plan's outcome and returns the governing verdict. A workspace with no policies
// pays nothing. The plan is read from the outcome the tofu driver already parsed
// out of `tofu show -json` and recorded on the plan job — a values-free view of
// what the plan changes (addresses, actions, attribute keys), so a policy
// decides on structure and no plan secret is ever in play. A plan that changed
// something the driver could not describe is failed closed: a plan a policy
// could not examine must not apply unseen.
func evalPlanPolicy(ctx context.Context, st *store.Store, out io.Writer,
	ws store.Workspace, jobID int64) (policy.Verdict, error) {
	policies, err := st.ListPlanPolicies(ctx, ws.Name, true)
	if err != nil {
		return policy.Verdict{}, err
	}
	if len(policies) == 0 {
		return policy.Verdict{}, nil
	}

	outcome, ok, err := planOutcome(ctx, st, jobID)
	if err != nil {
		return policy.Verdict{}, err
	}
	if !ok {
		return failClosed(out, "read the plan outcome", errNoPlanOutcome), nil
	}
	// A plan with changes but no described detail is a plan the driver's own
	// `show` could not render — the one case that must not slip through, since a
	// delete rule sees an empty list and would clear it.
	if outcome.Changes && len(outcome.Pending) == 0 && len(outcome.Drift) == 0 {
		return failClosed(out, "describe the plan's changes", errNoPlanDetail), nil
	}

	plan := policy.Plan(outcome.Changes,
		toPlanChanges(outcome.Pending), toPlanChanges(outcome.Drift))
	verdict, err := policy.Evaluate(ctx, policies, plan)
	if err != nil {
		return failClosed(out, "evaluate policies", err), nil
	}
	return verdict, nil
}

var (
	errNoPlanOutcome = errors.New("the plan recorded no outcome")
	errNoPlanDetail  = errors.New("the plan reported changes without resource detail")
)

// failClosed turns an evaluation that could not complete into a deny, so a plan
// no policy could decide is sealed unconsumable rather than waved through.
func failClosed(out io.Writer, stage string, err error) policy.Verdict {
	_, _ = fmt.Fprintf(out, "plan policy: could not %s (%v); sealing the plan denied\n", stage, err)
	return policy.Verdict{Policy: "policy-evaluation-failed", Action: store.PolicyDeny}
}

// planOutcome reads the structured plan outcome the tofu driver recorded on the
// plan job — the resource-level drift and pending changes it parsed from
// `tofu show -json`, with values already suppressed. It is what "read the
// plan-show JSON the driver produces" means here: the seam consumes the driver's
// parse rather than re-running tofu, which cannot describe a plan that deletes a
// resource whose provider is only in state.
func planOutcome(ctx context.Context, st *store.Store, jobID int64) (executor.TofuPlanOutcome, bool, error) {
	events, err := st.ListJobEvents(ctx, jobID)
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

// recordPlanVerdict writes a matched policy's decision onto the plan job's
// timeline, keyed to the sealed artifact. The record is the audit trail and the
// apply-time gate reads it back; it names the policy and action only, so a plan
// secret never reaches an event.
func recordPlanVerdict(ctx context.Context, st *store.Store, out io.Writer,
	jobID, artifactID int64, v policy.Verdict) error {
	payload, err := json.Marshal(map[string]planPolicyVerdict{
		"plan_policy": {Action: v.Action, Policy: v.Policy, Artifact: artifactID},
	})
	if err != nil {
		return fmt.Errorf("workspace: encode plan verdict: %w", err)
	}
	msg := fmt.Sprintf("plan policy: %s by %q", v.Action, v.Policy)
	if _, err := st.AppendJobEvent(ctx, store.JobEvent{
		JobID: jobID, Level: store.LevelSystem, Msg: msg, RawJSON: string(payload),
	}); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, msg)
	if v.Deny() {
		_, _ = fmt.Fprintln(out, "the sealed plan is unconsumable; a later apply of it will refuse")
	}
	return nil
}

// planVerdict reads back the policy verdict recorded on a sealed plan, or a
// zero verdict when none was recorded (no policies, or a plan applied by a path
// this control plane never sealed). It is what makes a deny plan unconsumable:
// the apply consults it before it enqueues anything.
func planVerdict(ctx context.Context, st *store.Store, artifact store.Artifact) (policy.Verdict, error) {
	if artifact.JobID == 0 {
		return policy.Verdict{}, nil
	}
	events, err := st.ListJobEvents(ctx, artifact.JobID)
	if err != nil {
		return policy.Verdict{}, err
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
		return policy.Verdict{Policy: envelope.PlanPolicy.Policy, Action: envelope.PlanPolicy.Action}, nil
	}
	return policy.Verdict{}, nil
}
