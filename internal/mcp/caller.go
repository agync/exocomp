package mcp

import (
	"context"
	"strconv"

	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// A transport authenticates; this package authorizes. Every dispatch takes the
// Caller the transport resolved, and the catalog a caller sees — plus the check
// re-run on every tools/call — is derived from that one value. Nothing in here
// reads process state: the HTTP transport resolves a bearer token to a Caller
// per request and hands it straight in (ADR 0014 §3).

// capability is what a tool needs of its caller. The three map 1:1 onto the
// ADR-0009 scopes: R is readonly-or-higher, W is operator, A is approve.
type capability int

const (
	capRead capability = iota
	capWrite
	capApprove
)

// scopeFor names the scope a capability needs, for the refusal message.
func (c capability) scopeFor() string {
	switch c {
	case capWrite:
		return store.ScopeOperator
	case capApprove:
		return store.ScopeApprove
	default:
		return store.ScopeReadonly
	}
}

// Decision is what a decision tool reports back: which approval settled, the
// outcome, and the terminal notice. It NEVER carries the decision capability —
// the single-use HMAC link is derived and consumed server-side, because handing
// one to an agent silently promotes it to approver (agent-interface §3.5).
type Decision struct {
	ApprovalID int64  `json:"approval_id"`
	Outcome    string `json:"outcome,omitempty"`
	Message    string `json:"message,omitempty"`
}

// DecideFunc settles one approval on behalf of the caller. It is supplied by the
// transport, not built here: the only implementation is the hardened decision
// path the HTTP write API already uses, so both machine faces enforce the same
// separation of duty through one body of code.
type DecideFunc func(ctx context.Context, approvalID int64, decision string) (Decision, error)

// Caller is the authenticated principal behind one MCP request.
type Caller struct {
	// Scope is the ADR-0009 scope the transport resolved (store.Scope*). An
	// unrecognized value authorizes nothing, so a zero Caller is inert.
	Scope string
	// Actor is what a write records on the job's event ledger, so an agent's
	// write is never read off the ledger as the operator's own run.
	Actor service.Actor
	// KeyNamespace scopes caller-chosen idempotency keys to this principal, so a
	// guessable key cannot swallow another caller's write.
	KeyNamespace string
	// Decide settles an approval. It is non-nil only on a transport that carries
	// a credential AND only for the approve scope; TokenCaller enforces that.
	Decide DecideFunc
}

// allows reports whether this caller may exercise a capability. The write and
// approve floors are DISJOINT: the scope that can raise a gate cannot release
// it, whichever face it comes in on.
func (c Caller) allows(need capability) bool {
	switch need {
	case capRead:
		switch c.Scope {
		case store.ScopeReadonly, store.ScopeOperator, store.ScopeApprove:
			return true
		}
		return false
	case capWrite:
		return c.Scope == store.ScopeOperator
	case capApprove:
		// A decision needs the scope AND a transport that can carry one out.
		return c.Scope == store.ScopeApprove && c.Decide != nil
	}
	return false
}

// TokenCaller is the principal behind a token-authenticated transport. Only the
// token's NAME and row id travel — never its secret (invariant 4) — and the
// decision seam is attached ONLY for the approve scope, so the disjointness
// holds at construction rather than depending on every call site remembering it.
func TokenCaller(scope, tokenName string, tokenID int64, decide DecideFunc) Caller {
	if scope != store.ScopeApprove {
		decide = nil
	}
	return Caller{
		Scope:        scope,
		Actor:        service.Actor{Kind: service.ActorKindToken, Name: tokenName, Face: faceName},
		KeyNamespace: "t" + strconv.FormatInt(tokenID, 10),
		Decide:       decide,
	}
}

// faceName is what a write records as the surface it came in on.
const faceName = "mcp"
