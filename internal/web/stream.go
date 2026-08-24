package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// streamPoll is how often the tail re-reads job_event. The events table is
// append-only and a job is a handful of lines a second at most, so a poll is
// simpler than a notify fan-out and costs nothing idle.
const streamPoll = 300 * time.Millisecond

// streamMaxLifetime bounds one attached stream regardless of the job, so a
// credential that is never revoked still cannot hold a socket forever. A client
// that still wants the tail re-attaches from its last sequence, which the
// follow path already does on a drop.
const streamMaxLifetime = 12 * time.Hour

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

	// The page rendered every event up to now; the stream carries what follows.
	var cursor int64
	if events, err := s.store.ListJobEvents(r.Context(), id); err == nil && len(events) > 0 {
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
				"seq": e.Seq, "ts": clock(e.Ts), "level": e.Level, "msg": e.Msg,
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
