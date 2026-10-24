package wire

import (
	"errors"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// Error codes. Each is mapped 1:1 onto a real store/reactive/auth sentinel so an
// agent branches on the code, never on a sentence.
const (
	CodeNotFound        = "not_found"        // store.ErrNotFound
	CodeConflict        = "conflict"         // store.ErrStateConflict
	CodeDuplicate       = "duplicate"        // store.ErrDuplicate
	CodeConcurrencyHeld = "concurrency_held" // store.ErrConcurrencyHeld
	CodeStalePlan       = "stale_plan"       // reactive.ErrStalePlan
	CodeKDFBusy         = "kdf_busy"         // web.ErrKDFBusy (auth boundary)
	// CodeStreamsOverCap is the boundary refusal for an over-cap SSE stream
	// (approvals or job tail). It is distinct from kdf_busy so a phone that
	// branches on the code does not treat a saturated stream as an auth-gate
	// backoff.
	CodeStreamsOverCap  = "streams_over_cap"
	CodeUnauthenticated = "unauthenticated" // web.ErrUnauthenticated (auth boundary)
	CodeForbidden       = "forbidden"       // scope check (auth boundary)
	// CodeRemoteUnsupported is the CLI's refusal of a command that has no
	// remote path while a remote daemon is selected (ErrRemoteUnsupported).
	CodeRemoteUnsupported = "remote_unsupported"
	// CodeInternal is the fallback for an error that matched no sentinel.
	CodeInternal = "internal"
)

// ErrRemoteUnsupported marks the CLI's refusal of a command that has no remote
// path while a remote daemon is selected. It lives here, beside its code,
// because the refusal is raised in cmd/exocomp, which no sentinel package sees.
var ErrRemoteUnsupported = errors.New("no remote path for this command")

// Exit codes are the primary agent signal (invariant 3): a weak model can branch
// on the process code alone. The table is the shipped exit-code→state mapping
// (internal/queue/queue.go:357-379, internal/executor/tofu.go) codified, not
// changed — ExitChangesPresent, for one, is tofu plan's -detailed-exitcode 2.
const (
	ExitOK               = 0 // ok
	ExitFailure          = 1 // generic failure: a run settled failed, a request errored
	ExitChangesPresent   = 2 // tofu plan found changes (-detailed-exitcode)
	ExitNotFound         = 3 // no such entity
	ExitAuth             = 4 // unauthenticated / forbidden
	ExitConflict         = 5 // conflict / concurrency held / locked
	ExitAwaitingApproval = 6 // a gated write landed pending
	ExitBusy             = 7 // transient: retry later (kdf_busy, streams_over_cap)
)

// hints are actionable, code-keyed, and stable (they carry no volatile detail),
// so they stay cache-friendly.
var hints = map[string]string{
	CodeNotFound:        "check the id or name; list the collection to see what exists",
	CodeConflict:        "re-read the entity's state; the transition you asked for is not legal from it",
	CodeDuplicate:       "the name or key is already taken; choose another or remove the existing one",
	CodeConcurrencyHeld: "another run holds this workspace; wait for it to finish or cancel it",
	CodeStalePlan:       "the workspace moved since the plan was saved; re-plan before applying",
	CodeKDFBusy:         "the auth gate is saturated; retry shortly",
	CodeStreamsOverCap:  "too many live streams; close a tab or retry shortly",
	CodeUnauthenticated: "supply a valid token or session",
	CodeForbidden:       "this token's scope is too low for the verb; mint one with a higher scope",
	CodeRemoteUnsupported: "pass --local to run the command against this machine's data dir, " +
		"or run it on the daemon's host",
}

// ErrorFor maps a Go error onto the taxonomy. It matches the store and reactive
// sentinels directly; the auth codes (kdf_busy/unauthenticated/forbidden) are
// produced at the web/api boundary, which cannot be imported here, via
// NewError. An unmatched error becomes CodeInternal.
func ErrorFor(err error) Error {
	if err == nil {
		return Error{Code: CodeInternal, Message: "nil error"}
	}
	code := CodeInternal
	switch {
	case errors.Is(err, store.ErrNotFound):
		code = CodeNotFound
	case errors.Is(err, store.ErrStateConflict):
		code = CodeConflict
	case errors.Is(err, store.ErrDuplicate):
		code = CodeDuplicate
	case errors.Is(err, store.ErrConcurrencyHeld):
		code = CodeConcurrencyHeld
	case errors.Is(err, reactive.ErrStalePlan):
		code = CodeStalePlan
	case errors.Is(err, ErrRemoteUnsupported):
		code = CodeRemoteUnsupported
	}
	return Error{Code: code, Message: err.Error(), Hint: hints[code]}
}

// NewError builds a typed error for a known code, filling the standard hint.
// The auth boundary uses it for the codes wire cannot map without importing web.
func NewError(code, message string) Error {
	return Error{Code: code, Message: message, Hint: hints[code]}
}

// ExitFor returns the process exit code for a Go error, via the taxonomy.
func ExitFor(err error) int {
	if err == nil {
		return ExitOK
	}
	return ExitForCode(ErrorFor(err).Code)
}

// ExitForCode maps an error code to its process exit code.
func ExitForCode(code string) int {
	switch code {
	case CodeNotFound:
		return ExitNotFound
	case CodeUnauthenticated, CodeForbidden:
		return ExitAuth
	case CodeConflict, CodeConcurrencyHeld, CodeStalePlan:
		return ExitConflict
	case CodeKDFBusy:
		return ExitBusy
	case CodeStreamsOverCap:
		return ExitBusy
	default:
		return ExitFailure
	}
}
