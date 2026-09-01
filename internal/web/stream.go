package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// streamPoll is how often the tail re-reads job_event. The events table is
// append-only and a job is a handful of lines a second at most, so a poll is
// simpler than a notify fan-out and costs nothing idle.
const streamPoll = 300 * time.Millisecond

// approvalsStreamPoll is how often the approvals tail re-reads the pending
// list. It is deliberately NOT streamPoll: an approval is a human-scale event,
// not a per-line log tail, and every pending-approvals stream hammers the same
// one-connection store pool as the queue and sequencer, so the 300ms job-tail
// cadence would buy nothing but contention.
const approvalsStreamPoll = 2 * time.Second

// approvalsStreamHeartbeat is how long the approvals tail may go without any
// write before it emits an SSE comment heartbeat (: keepalive). An idle live
// list would otherwise send nothing for hours and be silently killed by any
// intermediary's idle timeout.
const approvalsStreamHeartbeat = 30 * time.Second

// maxApprovalStreams bounds concurrent approvals streams per process. Each one
// polls ListApprovals every approvalsStreamPoll, so without a cap a fleet of
// stale tabs multiplies the load on the one-connection store pool forever.
const maxApprovalStreams = 8

// maxApprovalStreamsPerToken bounds concurrent approvals streams per token id,
// so one identity cannot hold every slot and a readonly session cannot starve
// an operator's stream. Four per identity still leaves the other half of the
// total for a second identity's tabs; an operator who actually needs more
// tailed surfaces than that closes a tab instead of stacking streams.
const maxApprovalStreamsPerToken = 4

// maxJobStreams bounds concurrent job-event tails per process. Each one polls
// ListJobEvents every streamPoll (300ms — six times the approvals cadence), so
// the same stale-tab fleet is worse here: a handful of stuck job pages hammers
// the one-connection store pool at the queue's own polling speed. The per-token
// sub-cap keeps one identity from holding every slot, as with approvals.
const maxJobStreams = 8

// maxJobStreamsPerToken bounds concurrent job tails per token id. Job tails
// are the more expensive stream (300ms poll against a 12h stuck-tab lifetime),
// so the per-token ceiling matches the approvals one for the same reason: one
// identity cannot crowd out another's live tail.
const maxJobStreamsPerToken = 4

// streamMaxLifetime bounds one attached stream regardless of the job, so a
// credential that is never revoked still cannot hold a socket forever. A client
// that still wants the tail re-attaches from its last sequence, which the
// follow path already does on a drop.
const streamMaxLifetime = 12 * time.Hour

// sseTS formats a timestamp identically to the wire layer's own rfc3339
// (internal/wire/schema.go: t.UTC().Format(time.RFC3339)), so a client parsing
// both an SSE line and the REST job/event views it mirrors uses one timestamp
// parser instead of two. It duplicates the one-line format rather than
// exporting wire's helper — the wire package's job is projecting rows, not
// serving SSE frames.
func sseTS(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// approvalsFP hashes the wire projection of a pending-approvals list — the
// exact bytes tailApprovals diffs and the /approvals page renders — so the page
// and the stream can agree on whether the connect-time state is already on
// screen. It must be computed from the wire payload, not the page's approvalRow
// (whose fields differ from ApprovalView), or the two could never match.
func approvalsFP(rows []store.Approval) string {
	payload, _ := json.Marshal(wire.NewApprovalList(rows))
	return approvalsFPHash(payload)
}

// approvalsFPHash fingerprints one stream payload, the same bytes tailApprovals
// diffs, so ?fp= and the first read compare on one basis.
func approvalsFPHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// handleJobStream tails one job's events over SSE for the browser surface. It
// replays nothing the page already rendered: the client opens the stream after
// the server-rendered tail, and the stream is authoritative from there. A
// missing job or an unsupported ResponseWriter renders the browser's own
// not-found/500; everything past that is the shared tail in tailJobEvents.
func (s *Server) handleJobStream(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, err := s.store.GetJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s.tailJobEvents(w, r, id, flusher, false)
}

// tailJobEvents is the append-only job_event tail shared by the browser SSE
// surface (handleJobStream) and the API's job-follow route (apiJobStream): the
// same monotonic Seq cursor and the same done-on-terminal ending, so the two
// surfaces cannot drift into different streaming behavior. It also enforces the
// shared job-stream cap, with the same per-token sub-cap as tailApprovals —
// job tails poll six times as often as approvals, so a fleet of stuck job pages
// is the worse offender against the one-connection store pool. Existence, auth,
// and error rendering are the caller's job — this assumes the job exists and
// the response is already cleared to stream. api tells the cap's refusal which
// face it answers, exactly like tailApprovals: the API route gets the wire
// error envelope plus a Retry-After, the browser route a plain 503.
func (s *Server) tailJobEvents(w http.ResponseWriter, r *http.Request, id int64, flusher http.Flusher, api bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// The identity resolved at attach; each revalidation re-reads its row, so a
	// revoke or an expiry ends the stream rather than being invisible to it.
	// The credential epoch is pinned at attach too: re-reading only the row's
	// Active flag would let a rotated token — whose row stays Active — keep
	// reading the tail across the rotation meant to cut it off.
	ident, hasIdent := identityOf(r.Context())
	var attachedEpoch int64
	if hasIdent {
		attachedEpoch = ident.token.CredentialEpoch()
	}

	// The cap is enforced here, in the shared funnel, so the browser route and
	// the API route cannot each outrun it. Per-token is charged first so a
	// readonly flood of attaches cannot push the global count over the ceiling
	// transiently; the global-refusal path undoes the per-token charge it just
	// made. The decrement is deferred so every exit path releases a slot.
	if hasIdent {
		s.jobStreamsMu.Lock()
		if s.jobStreamsByToken[ident.token.ID] >= maxJobStreamsPerToken {
			s.jobStreamsMu.Unlock()
			s.refuseJobStream(w, r, api)
			return
		}
		s.jobStreamsByToken[ident.token.ID]++
		s.jobStreamsMu.Unlock()
	}
	if n := s.jobStreams.Add(1); n > maxJobStreams {
		s.jobStreams.Add(-1)
		if hasIdent {
			s.jobStreamsMu.Lock()
			if n := s.jobStreamsByToken[ident.token.ID]; n <= 1 {
				delete(s.jobStreamsByToken, ident.token.ID)
			} else {
				s.jobStreamsByToken[ident.token.ID] = n - 1
			}
			s.jobStreamsMu.Unlock()
		}
		s.refuseJobStream(w, r, api)
		return
	}
	defer func() {
		s.jobStreams.Add(-1)
		if hasIdent {
			s.jobStreamsMu.Lock()
			if n := s.jobStreamsByToken[ident.token.ID]; n <= 1 {
				delete(s.jobStreamsByToken, ident.token.ID)
			} else {
				s.jobStreamsByToken[ident.token.ID] = n - 1
			}
			s.jobStreamsMu.Unlock()
		}
	}()

	// The client passes the Seq of the last event its own render included
	// (?since=); the stream backfills everything after that, not just what is
	// newest at connect time — the gap between page-render and SSE-connect is
	// exactly what a fast-terminating job falls into otherwise. A missing or
	// unparseable since (the API follow route, older clients) falls back to
	// the latest event at connect, unchanged from before.
	var cursor int64
	if since := r.URL.Query().Get("since"); since != "" {
		cursor, _ = strconv.ParseInt(since, 10, 64)
	} else if events, err := s.store.ListJobEvents(r.Context(), id); err == nil && len(events) > 0 {
		cursor = events[len(events)-1].Seq
	}

	ticker := time.NewTicker(streamPoll)
	defer ticker.Stop()

	deadline := s.now().Add(streamMaxLifetime)

	for {
		// Re-resolve before every read: auth used to be resolved once, at attach,
		// so a leaked token's holder kept reading a job's log across the revoke
		// meant to cut it off. This is a token-row read with no
		// KDF, so it costs one indexed SELECT per poll.
		now := s.now()
		if !now.Before(deadline) {
			return
		}
		if ident.via == "session" {
			// A browser session is re-resolved per poll, not just its token
			// row: resolve() also enforces the session's own expiry stamp and
			// the credential-epoch pin, so a rotated token — whose row stays
			// Active — ends the stream the way a revoke does. No KDF runs on
			// the cookie path, so the per-poll cost is unchanged.
			if _, err := s.resolve(r); err != nil {
				return
			}
		} else if hasIdent {
			row, err := s.authenticateRow(r.Context(), ident.token.ID, "", now)
			if err != nil || row.CredentialEpoch() != attachedEpoch {
				return
			}
		}

		events, err := s.store.ListJobEvents(r.Context(), id)
		if err != nil {
			return
		}
		for _, e := range events {
			if e.Seq <= cursor {
				continue
			}
			cursor = e.Seq
			payload, _ := json.Marshal(map[string]any{
				"seq": e.Seq, "ts": sseTS(e.Ts), "level": e.Level, "msg": e.Msg,
			})
			if _, err := fmt.Fprintf(w, "event: line\ndata: %s\n\n", payload); err != nil {
				return
			}
		}

		current, err := s.store.GetJob(r.Context(), id)
		if err == nil && current.State.Terminal() {
			_, _ = fmt.Fprint(w, "event: done\ndata: {}\n\n")
			flusher.Flush()
			return
		}
		flusher.Flush()

		select {
		case <-r.Context().Done():
			return
		case <-s.streamStop:
			return
		case <-ticker.C:
		}
	}
}

// tailApprovals streams the pending-approvals list as SSE, the approvals
// analogue of tailJobEvents: the same re-resolve-identity-per-poll revocation
// property, poll ticker, and streamMaxLifetime deadline. Unlike a job there is
// no terminal state to end the stream on — it lives until the client leaves or
// the lifetime cap fires. Existence has nothing to check (there is no single
// resource, only the pending set) and there is no ?since= cursor to seed from,
// so — unlike tailJobEvents — this owns setting the SSE headers itself; the
// caller (apiApprovalStream) only checks the ResponseWriter can flush.
//
// It is the shared funnel for BOTH approvals surfaces (handleApprovalStream and
// apiApprovalStream), so the concurrency cap and the ?fp= connect logic live
// here, not in a handler. api tells the cap's refusal which face it answers:
// the API route gets the wire error envelope plus a Retry-After, the browser
// route a 503 the approvalsLive script turns into the stale banner.
func (s *Server) tailApprovals(w http.ResponseWriter, r *http.Request, flusher http.Flusher, api bool) {
	// The identity is resolved before the cap so the per-identity sub-cap can
	// key on the token id: one identity must not be able to hold every slot a
	// second stream of another identity would need.
	ident, hasIdent := identityOf(r.Context())
	// The credential epoch is pinned at attach, like tailJobEvents: re-reading
	// only the row's Active flag would let a rotated bearer token keep its
	// stream up to the lifetime cap.
	var attachedEpoch int64
	if hasIdent {
		attachedEpoch = ident.token.CredentialEpoch()
	}

	// The cap is enforced here, in the shared funnel, so the browser route and
	// the API route cannot each outrun it. The per-token check runs first so a
	// readonly flood of attaches cannot push the global count over the ceiling
	// transiently; the global-refusal path undoes the per-token charge it just
	// made. The decrement is deferred so every exit path — cap refusal,
	// deadline, auth failure, store error, client leave, shutdown — releases a
	// slot.
	if hasIdent {
		s.approvalStreamsMu.Lock()
		if s.approvalStreamsByToken[ident.token.ID] >= maxApprovalStreamsPerToken {
			s.approvalStreamsMu.Unlock()
			s.refuseApprovalStream(w, r, api)
			return
		}
		s.approvalStreamsByToken[ident.token.ID]++
		s.approvalStreamsMu.Unlock()
	}
	if n := s.approvalStreams.Add(1); n > maxApprovalStreams {
		s.approvalStreams.Add(-1)
		if hasIdent {
			s.approvalStreamsMu.Lock()
			if n := s.approvalStreamsByToken[ident.token.ID]; n <= 1 {
				delete(s.approvalStreamsByToken, ident.token.ID)
			} else {
				s.approvalStreamsByToken[ident.token.ID] = n - 1
			}
			s.approvalStreamsMu.Unlock()
		}
		s.refuseApprovalStream(w, r, api)
		return
	}
	defer func() {
		s.approvalStreams.Add(-1)
		if hasIdent {
			s.approvalStreamsMu.Lock()
			if n := s.approvalStreamsByToken[ident.token.ID]; n <= 1 {
				delete(s.approvalStreamsByToken, ident.token.ID)
			} else {
				s.approvalStreamsByToken[ident.token.ID] = n - 1
			}
			s.approvalStreamsMu.Unlock()
		}
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ticker := time.NewTicker(approvalsStreamPoll)
	defer ticker.Stop()
	// The heartbeat keeps an idle live list audible to intermediaries: nothing
	// else is written for hours on a quiet pending set, and any idle timeout
	// kills a stream a silent EventSource would never notice or rebuild.
	keepalive := time.NewTicker(s.heartbeat)
	defer keepalive.Stop()

	deadline := s.now().Add(streamMaxLifetime)

	// wantFP is the fingerprint of the pending list this page already rendered
	// (handleApprovals → vm.ApprovalFP). On connect a match means the snapshot
	// would just reload the page, so the stream stays silent until the first
	// real change; a differ — or an absent ?fp=, the API route — sends the
	// snapshot as before, so the render-to-connect window can never silently
	// drop a change.
	wantFP := r.URL.Query().Get("fp")
	// last is the previously sent payload; comparing the full projected bytes
	// (rather than just ids/states) means a field-only change — a decision
	// clock ticking a plan_serial, a reason edit — still counts as a change,
	// not just membership in the pending set. nil (not "[]") until the first
	// send, so an empty pending set still produces the one initial snapshot.
	var last []byte
	lastWrite := s.now()
	for {
		now := s.now()
		if !now.Before(deadline) {
			return
		}
		if ident.via == "session" {
			// A browser session is re-resolved per poll, not just its token
			// row: resolve() also enforces the session's own expiry stamp and
			// the credential-epoch pin, so a rotated token — whose row stays
			// Active — ends the stream the way a revoke does. No KDF runs on
			// the cookie path, so the per-poll cost is unchanged.
			if _, err := s.resolve(r); err != nil {
				return
			}
		} else if hasIdent {
			row, err := s.authenticateRow(r.Context(), ident.token.ID, "", now)
			if err != nil || row.CredentialEpoch() != attachedEpoch {
				return
			}
		}

		rows, err := s.store.ListApprovals(r.Context(), true)
		if err != nil {
			return
		}
		// The same projection apiApprovals serves, so a phone that already
		// parses GET /api/v1/approvals reuses that parsing here: no secrets,
		// no plan text, just approval metadata.
		payload, err := json.Marshal(wire.NewApprovalList(rows))
		if err != nil {
			return
		}
		if last == nil {
			// First read. A ?fp= that matches this exact payload means the page
			// already rendered it, so sending the snapshot would reload a list
			// that is already current; record the baseline and wait for a
			// change. The bare flush still commits the 200 + SSE headers so
			// the client sees an established connection. Differing or absent fp
			// sends the snapshot as before.
			if wantFP != "" && approvalsFPHash(payload) == wantFP {
				last = payload
				flusher.Flush()
			} else {
				last = payload
				if _, err := fmt.Fprintf(w, "event: approvals\ndata: %s\n\n", payload); err != nil {
					return
				}
				lastWrite = now
				flusher.Flush()
			}
		} else if !bytes.Equal(payload, last) {
			last = payload
			if _, err := fmt.Fprintf(w, "event: approvals\ndata: %s\n\n", payload); err != nil {
				return
			}
			lastWrite = now
			flusher.Flush()
		}

		select {
		case <-r.Context().Done():
			return
		case <-s.streamStop:
			return
		case <-ticker.C:
		case <-keepalive.C:
			// Only when the whole window saw no write: a data event a moment
			// ago is keepalive enough, and doubling up buys nothing.
			if s.now().Sub(lastWrite) >= s.heartbeat {
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

// refuseApprovalStream answers an over-cap approvals stream the way its face
// reads it: the API route gets the wire error envelope plus a Retry-After so
// an agent backs off instead of hammering, the browser route a plain 503 whose
// EventSource onerror the approvalsLive script turns into the visible stale
// banner (a refused stream must never be silently dead). The refusal carries
// CodeStreamsOverCap, not kdf_busy — a phone branching on the code must not
// treat a saturated stream as an auth-gate backoff.
func (s *Server) refuseApprovalStream(w http.ResponseWriter, _ *http.Request, api bool) {
	if api {
		w.Header().Set("Retry-After", "30")
		s.apiError(w, http.StatusServiceUnavailable,
			wire.NewError(wire.CodeStreamsOverCap, "too many approvals streams; retry shortly"))
		return
	}
	http.Error(w, "too many approvals streams", http.StatusServiceUnavailable)
}

// refuseJobStream answers an over-cap job tail the same way refuseApprovalStream
// answers an approvals one: the API face (jobs follow --remote) gets the wire
// envelope plus a Retry-After so the follow treats the refusal as the retryable
// busy it is (ExitBusy), the browser face a plain 503.
func (s *Server) refuseJobStream(w http.ResponseWriter, _ *http.Request, api bool) {
	if api {
		w.Header().Set("Retry-After", "30")
		s.apiError(w, http.StatusServiceUnavailable,
			wire.NewError(wire.CodeStreamsOverCap, "too many job tails; retry shortly"))
		return
	}
	http.Error(w, "too many job tails", http.StatusServiceUnavailable)
}
