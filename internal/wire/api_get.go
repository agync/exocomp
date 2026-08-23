package wire

import "exocomp.dev/exocomp/internal/store"

// This file adds the single-resource read projections the operator HTTP API
// needs and that the CLI never grew (the CLI exposes these as list verbs and an
// html/trace surface, not a values-free `get`). It lives in its own file so the
// Wave-0 projections in schema.go stay owned by their lane; nothing here edits
// them. The field mappings deliberately mirror NewApprovalList/NewEventList so a
// single-object `.get` and a one-element `.list` project a row identically.

// Kinds for the single-resource GETs. They read `.get`, matching KindJob, so an
// agent tells one object from a list by kind alone.
const (
	KindApproval = "approval.get"
	KindEvent    = "event.get"
)

// NewApprovalView projects one approval. It mirrors NewApprovalList's element
// mapping exactly; the token hash — the capability material — never travels.
func NewApprovalView(a store.Approval) ApprovalView {
	return ApprovalView{
		ID: a.ID, JobID: a.JobID, State: string(a.State), Workspace: a.Workspace,
		PlanSerial: a.PlanSerial, RequestedAt: rfc3339(a.RequestedAt),
		ExpiresAt: rfc3339(a.ExpiresAt), DecidedAt: rfc3339p(a.DecidedAt),
		DecidedVia: a.DecidedVia, Reason: a.Reason,
	}
}

// NewEventView projects one received event. It mirrors NewEventList's element
// mapping exactly; the normalized/raw payloads and the per-condition trace
// (which can carry values) are omitted, as they are from the list.
func NewEventView(e store.Event) EventView {
	return EventView{
		ID: e.ID, Source: e.Source, ReceivedAt: rfc3339(e.ReceivedAt), DedupKey: e.DedupKey,
		MatchedRule: e.MatchedRule, Outcome: string(e.Outcome), Reason: e.Reason, JobID: e.JobID,
	}
}
