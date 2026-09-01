package web

import (
	"bytes"
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
	s.tailJobEvents(w, r, id, flusher)
}

// tailJobEvents is the append-only job_event tail shared by the browser SSE
// surface (handleJobStream) and the API's job-follow route (apiJobStream): the
// same monotonic Seq cursor and the same done-on-terminal ending, so the two
// surfaces cannot drift into different streaming behavior. Existence, auth,
// and error rendering are the caller's job — this assumes the job exists and
// the response is already cleared to stream.
func (s *Server) tailJobEvents(w http.ResponseWriter, r *http.Request, id int64, flusher http.Flusher) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

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

	// The identity resolved at attach; each revalidation re-reads its row, so a
	// revoke or an expiry ends the stream rather than being invisible to it.
	ident, hasIdent := identityOf(r.Context())
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
		if hasIdent {
			if _, err := s.authenticateRow(r.Context(), ident.token.ID, "", now); err != nil {
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
// here, not in a handler.
func (s *Server) tailApprovals(w http.ResponseWriter, r *http.Request, flusher http.Flusher) {
	// The cap is enforced here, in the shared funnel, so the browser route and
	// the API route cannot each outrun it. The decrement is deferred so every
	// exit path — cap refusal, deadline, auth failure, store error, client
	// leave, shutdown — releases a slot.
	if n := s.approvalStreams.Add(1); n > maxApprovalStreams {
		s.approvalStreams.Add(-1)
		http.Error(w, "too many streams", http.StatusServiceUnavailable)
		return
	}
	defer s.approvalStreams.Add(-1)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ticker := time.NewTicker(approvalsStreamPoll)
	defer ticker.Stop()

	ident, hasIdent := identityOf(r.Context())
	deadline := s.now().Add(streamMaxLifetime)

	// last is the previously sent payload; comparing the full projected bytes
	// (rather than just ids/states) means a field-only change — a decision
	// clock ticking a plan_serial, a reason edit — still counts as a change,
	// not just membership in the pending set. nil (not "[]") until the first
	// send, so an empty pending set still produces the one initial snapshot.
	var last []byte
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
			if _, err := s.authenticateRow(r.Context(), ident.token.ID, "", now); err != nil {
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
		if last == nil || !bytes.Equal(payload, last) {
			last = payload
			if _, err := fmt.Fprintf(w, "event: approvals\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
