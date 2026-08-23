package main

import (
	"context"
	"fmt"
	"io"

	"exocomp.dev/exocomp/internal/policy"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// openPlanForRun writes a saved plan out in the clear for the one run that
// consumes it and returns the path with the function that removes it. The seam
// lives in internal/service so the daemon opens a sealed plan on release the
// same way; this wrapper keeps the CLI call site and its signature.
func openPlanForRun(env *secrets.Envelope, privateRoot, path string) (string, func(), error) {
	return service.OpenPlanForRun(planEnvelope(env), privateRoot, path)
}

// planEnvelope adapts a possibly-nil *secrets.Envelope to the service seam's
// interface without turning a nil envelope into a non-nil interface value (which
// would defeat the nil check the seam relies on).
func planEnvelope(env *secrets.Envelope) service.PlanEnvelope {
	if env == nil {
		return nil
	}
	return env
}

// evalPlanPolicy evaluates a workspace's enabled policies against a sealed
// plan's outcome and returns the governing verdict, printing the fail-closed
// notice when evaluation could not complete. The transport-free logic lives in
// internal/service.
func evalPlanPolicy(ctx context.Context, st *store.Store, out io.Writer,
	ws store.Workspace, jobID int64) (policy.Verdict, error) {
	res, err := service.New(st, "", nil).EvalPlanPolicy(ctx, ws, jobID)
	if err != nil {
		return policy.Verdict{}, err
	}
	if res.FailNotice != "" {
		_, _ = fmt.Fprintln(out, res.FailNotice)
	}
	return res.Verdict, nil
}

// recordPlanVerdict writes a matched policy's decision onto the plan job's
// timeline and prints the notice. The store write lives in internal/service.
func recordPlanVerdict(ctx context.Context, st *store.Store, out io.Writer,
	jobID, artifactID int64, v policy.Verdict) error {
	msg, err := service.New(st, "", nil).RecordPlanVerdict(ctx, jobID, artifactID, v)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, msg)
	if v.Deny() {
		_, _ = fmt.Fprintln(out, "the sealed plan is unconsumable; a later apply of it will refuse")
	}
	return nil
}

// planVerdict reads back the policy verdict recorded on a sealed plan, or a zero
// verdict when none was recorded. The read lives in internal/service.
func planVerdict(ctx context.Context, st *store.Store, artifact store.Artifact) (policy.Verdict, error) {
	return service.New(st, "", nil).PlanVerdict(ctx, artifact)
}
