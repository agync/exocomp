package mcp

import (
	"context"
	"encoding/json"

	"exocomp.dev/exocomp/internal/reactive"
)

// The A column: approve_job and reject_job. They exist only for a caller whose
// scope is `approve` AND whose transport supplied a decision seam, which today
// means the token-carrying HTTP face — stdio has neither.
//
// Two properties make this safe to offer at all, and both are structural rather
// than a rule this file remembers:
//
//   - W and A are DISJOINT scopes (Caller.allows). A caller advertised these
//     tools is advertised no write tool, so no principal can raise a gate with
//     workspace_apply and then release it here.
//   - The decision itself is NOT implemented here. Caller.Decide is the same
//     fail-closed path the HTTP write API's decision route runs, which derives
//     the single-use capability server-side and refuses a principal the gated
//     job's ledger records as its actor. This file decodes an id and reports an
//     outcome; it can neither mint a capability nor skip that refusal.
//
// The result carries approval id, outcome, and the terminal notice — never the
// decision link, which is a capability, not a datum (agent-interface §3.5).

// kindDecision is the wire kind of a settled decision, matching the HTTP write
// API's kind for the same event so an agent parses one shape on either face.
const kindDecision = "approval.decision"

// approveTools is the A column, empty for a caller with no decision seam.
func (s *Server) approveTools() []tool {
	return []tool{
		{"approve_job", "Approve one pending approval, releasing the job it gates.",
			object(map[string]any{"approval_id": intProp("the approval id")}, "approval_id"),
			s.runApproveJob},
		{"reject_job", "Reject one pending approval; the job it gates is withdrawn.",
			object(map[string]any{"approval_id": intProp("the approval id")}, "approval_id"),
			s.runRejectJob},
	}
}

func (s *Server) runApproveJob(ctx context.Context, c Caller, args json.RawMessage) (string, any, error) {
	return s.toolDecide(ctx, c, args, reactive.DecisionApprove)
}

func (s *Server) runRejectJob(ctx context.Context, c Caller, args json.RawMessage) (string, any, error) {
	return s.toolDecide(ctx, c, args, reactive.DecisionReject)
}

// toolDecide settles one approval through the caller's decision seam.
func (s *Server) toolDecide(ctx context.Context, c Caller, args json.RawMessage, decision string) (string, any, error) {
	var a struct {
		ApprovalID int64 `json:"approval_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.ApprovalID <= 0 {
		return "", nil, invalidArg("approval_id must be a positive integer")
	}
	if c.Decide == nil {
		// handleToolsCall refuses capApprove without a seam, so this is
		// unreachable through the dispatcher — it is here so the seam can never
		// be nil-dereferenced into a panic that a caller could read as a fault.
		return "", nil, forbidden("this transport carries no decision capability")
	}
	res, err := c.Decide(ctx, a.ApprovalID, decision)
	if err != nil {
		return "", nil, err
	}
	return kindDecision, res, nil
}
