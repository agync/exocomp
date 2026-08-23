package service

import (
	"context"

	"exocomp.dev/exocomp/internal/reactive"
)

// DecideInput is one answer to a decision link: which approval, the decision
// (reactive.DecisionApprove or DecisionReject), the presented capability token,
// and the surface it arrived on (recorded on the approval, e.g. "web:<token>"
// or "cli").
type DecideInput struct {
	ApprovalID int64
	Decision   string
	Token      string
	Via        string
}

// DecideResult carries the engine's decision: the settled approval, the outcome,
// and the terminal notice a face renders.
type DecideResult struct {
	Decision reactive.Decision
}

// DecideApproval resolves one decision link through the reactive engine — the
// single place a human's answer enters the system, whatever surface asked. The
// engine is injected because it also holds the queue an approval releases into,
// which is a property of the running process, not of this input. Decide stays
// fail-closed: the token is checked first, then the TTL, so an expired link can
// never release a job; this verb adds nothing that could change that.
func (s *Service) DecideApproval(ctx context.Context, eng *reactive.Engine, in DecideInput) (DecideResult, error) {
	decision, err := eng.Decide(ctx, in.ApprovalID, in.Decision, in.Token, in.Via)
	if err != nil {
		return DecideResult{Decision: decision}, err
	}
	return DecideResult{Decision: decision}, nil
}
