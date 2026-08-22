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

// handleJobStream tails one job's events over SSE. It replays nothing the page
// already rendered: the client opens the stream after the server-rendered tail,
// and the stream is authoritative from there. It ends with a `done` event when
// the job reaches a terminal state, so the browser closes the connection
// instead of polling a finished job forever.
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

	for {
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
