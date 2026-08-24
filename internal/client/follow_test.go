package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/wire"
)

// jobStub models the daemon's two job surfaces the follow reads: the append-only
// event log (GET .../events?since=) and the SSE tail (GET .../stream). Events are
// appended over time, exactly as a running job's are, so a test can drop the
// stream mid-job and assert what the resume recovers.
type jobStub struct {
	mu       sync.Mutex
	appended []Event
	// streams counts stream attaches, and sinces records the ?since each carried.
	streams int
	sinces  []string
	lastIDs []string
	// events counts reads of the append-only event log (the follow's backfill).
	events int
	// onStream is called per attach with the response writer; it decides what to
	// emit and whether to drop the connection (return false).
	onStream func(t *testing.T, s *jobStub, w http.ResponseWriter, attempt int) (keepOpen bool)
}

func (s *jobStub) append(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appended = append(s.appended, ev)
}

// waitBackfills blocks until the event log has been read n times, so a test can
// order "the resume backfilled" against "the daemon appended". It gives up after
// a bounded wait rather than hanging the suite.
func (s *jobStub) waitBackfills(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := s.events
		s.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("event log was not read %d times", n)
}

func (s *jobStub) since(v int64) []wire.JobEventView {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events++
	out := []wire.JobEventView{}
	for _, e := range s.appended {
		if e.Seq > v {
			out = append(out, wire.JobEventView{Seq: e.Seq, Ts: e.Ts, Level: e.Level, Msg: e.Msg})
		}
	}
	return out
}

func (s *jobStub) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/jobs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		var since int64
		if v := r.URL.Query().Get("since"); v != "" {
			since, _ = strconv.ParseInt(v, 10, 64)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = wire.WriteSuccess(w, wire.KindJobEvents, s.since(since))
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.streams++
		attempt := s.streams
		s.sinces = append(s.sinces, r.URL.Query().Get("since"))
		s.lastIDs = append(s.lastIDs, r.Header.Get("Last-Event-ID"))
		s.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if keep := s.onStream(t, s, w, attempt); !keep {
			// Drop the connection mid-stream: hijack and close, which is what a
			// tailnet blip looks like to the client.
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		}
	})
	return mux
}

func sseLine(t *testing.T, w http.ResponseWriter, ev Event) {
	t.Helper()
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(w, "event: line\ndata: %s\n\n", payload)
	_ = http.NewResponseController(w).Flush()
}

func sseDone(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	_, _ = fmt.Fprint(w, "event: done\ndata: {}\n\n")
	_ = http.NewResponseController(w).Flush()
}

func ev(seq int64, msg string) Event {
	return Event{Seq: seq, Ts: "2026-08-23T10:00:00Z", Level: "stdout", Msg: msg}
}

func TestFollowJobStreamsToDone(t *testing.T) {
	stub := &jobStub{onStream: func(t *testing.T, s *jobStub, w http.ResponseWriter, _ int) bool {
		for i := int64(1); i <= 3; i++ {
			e := ev(i, fmt.Sprintf("line %d", i))
			s.append(e)
			sseLine(t, w, e)
		}
		sseDone(t, w)
		return true
	}}
	srv, fp := tlsServer(t, stub.handler(t))
	c := newTestClient(t, srv, fp)

	var got []Event
	if err := c.FollowJob(context.Background(), 7, 0, func(e Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("FollowJob: %v", err)
	}
	assertSeqs(t, got, 1, 2, 3)
	if got[0].Msg != "line 1" || got[2].Level != "stdout" {
		t.Fatalf("events = %+v", got)
	}
}

// The GATE for the reconnect: the stream drops after event 2, an event 3 lands
// while nothing is attached, and the ONE reconnect resumes from the last Seq —
// so the caller sees 1,2,3,4 in order, each exactly once.
func TestFollowJobReconnectsAndResumes(t *testing.T) {
	stub := &jobStub{onStream: func(t *testing.T, s *jobStub, w http.ResponseWriter, attempt int) bool {
		switch attempt {
		case 1:
			for i := int64(1); i <= 2; i++ {
				e := ev(i, fmt.Sprintf("line %d", i))
				s.append(e)
				sseLine(t, w, e)
			}
			// Wait out this attach's own backfill before appending, so event 3 is
			// unambiguously an event that landed while NOTHING was attached: only
			// the resume's backfill can recover it, because the daemon's stream
			// replays nothing from before it attached.
			s.waitBackfills(t, 1)
			s.append(ev(3, "line 3"))
			return false
		default:
			e := ev(4, "line 4")
			s.append(e)
			sseLine(t, w, e)
			sseDone(t, w)
			return true
		}
	}}
	srv, fp := tlsServer(t, stub.handler(t))
	c := newTestClient(t, srv, fp)

	var got []Event
	if err := c.FollowJob(context.Background(), 7, 0, func(e Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("FollowJob: %v", err)
	}
	assertSeqs(t, got, 1, 2, 3, 4)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.streams != 2 {
		t.Fatalf("stream attaches = %d, want 2", stub.streams)
	}
	if stub.sinces[1] != "2" || stub.lastIDs[1] != "2" {
		t.Fatalf("resume did not carry the cursor: since=%q Last-Event-ID=%q",
			stub.sinces[1], stub.lastIDs[1])
	}
}

// The reconnect budget is ONE: a stream that keeps dropping is a fault the
// operator sees, not a loop that hides it.
func TestFollowJobGivesUpAfterOneReconnect(t *testing.T) {
	stub := &jobStub{onStream: func(t *testing.T, s *jobStub, w http.ResponseWriter, _ int) bool {
		e := ev(1, "line 1")
		s.append(e)
		sseLine(t, w, e)
		return false
	}}
	srv, fp := tlsServer(t, stub.handler(t))
	c := newTestClient(t, srv, fp)

	err := c.FollowJob(context.Background(), 7, 0, func(Event) error { return nil })
	if err == nil {
		t.Fatal("a stream that never settles must fail")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.streams != 2 {
		t.Fatalf("stream attaches = %d, want 2 (one attach + one reconnect)", stub.streams)
	}
}

// A follow that resumes from a Seq does not re-read what the caller already has.
func TestFollowJobSinceSkipsSeenEvents(t *testing.T) {
	stub := &jobStub{onStream: func(t *testing.T, _ *jobStub, w http.ResponseWriter, _ int) bool {
		sseLine(t, w, ev(1, "old"))
		sseLine(t, w, ev(2, "old"))
		sseLine(t, w, ev(3, "new"))
		sseDone(t, w)
		return true
	}}
	stub.append(ev(1, "old"))
	stub.append(ev(2, "old"))
	srv, fp := tlsServer(t, stub.handler(t))
	c := newTestClient(t, srv, fp)

	var got []Event
	if err := c.FollowJob(context.Background(), 7, 2, func(e Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("FollowJob: %v", err)
	}
	assertSeqs(t, got, 3)
}

// The wire error path: a follow of an unknown job fails with the same typed
// APIError a read would, so the CLI exits on the same code.
func TestFollowJobWireError(t *testing.T) {
	srv, fp := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = wire.WriteErrorValue(w, wire.NewError(wire.CodeNotFound, "no job 999"))
	}))
	c := newTestClient(t, srv, fp)

	err := c.FollowJob(context.Background(), 999, 0, func(Event) error { return nil })
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != wire.CodeNotFound || apiErr.ExitCode() != wire.ExitNotFound {
		t.Fatalf("error = %+v", apiErr)
	}
}

// A callback failure ends the follow at once: reconnecting would re-deliver
// events to a consumer that already refused one.
func TestFollowJobStopsOnCallbackError(t *testing.T) {
	boom := errors.New("consumer closed")
	stub := &jobStub{onStream: func(t *testing.T, s *jobStub, w http.ResponseWriter, _ int) bool {
		e := ev(1, "line 1")
		s.append(e)
		sseLine(t, w, e)
		sseLine(t, w, ev(2, "line 2"))
		sseDone(t, w)
		return true
	}}
	srv, fp := tlsServer(t, stub.handler(t))
	c := newTestClient(t, srv, fp)

	err := c.FollowJob(context.Background(), 7, 0, func(Event) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("want the callback's own error, got %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.streams != 1 {
		t.Fatalf("stream attaches = %d, want 1 (no reconnect after a callback failure)", stub.streams)
	}
}

// A cancelled context ends the follow with ctx.Err(), not a reconnect loop.
func TestFollowJobHonoursContextCancel(t *testing.T) {
	stub := &jobStub{onStream: func(t *testing.T, _ *jobStub, w http.ResponseWriter, _ int) bool {
		sseLine(t, w, ev(1, "line 1"))
		// Hold the stream open without settling, as a long apply does.
		time.Sleep(2 * time.Second)
		return true
	}}
	srv, fp := tlsServer(t, stub.handler(t))
	c := newTestClient(t, srv, fp)

	ctx, cancel := context.WithCancel(context.Background())
	err := c.FollowJob(ctx, 7, 0, func(Event) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// A peer that accepts the TCP connection but never speaks TLS models a
// blackholed / dead-tailnet path from the connect phase's point of view: the
// handshake never completes. The stream client has no overall Timeout (a
// legitimate follow can run for as long as the job does), so without a bound
// on the connect phase itself this hangs forever instead of failing fast.
func TestOpenStreamConnectIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and hold the connection open without ever completing
			// the TLS handshake — the same shape as a peer that swallows
			// packets after the TCP handshake.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	c, err := New(Config{Endpoint: ln.Addr().String(), Fingerprint: "AA", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.openStream(context.Background(), 1, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want a connect error for a peer that never completes TLS, got nil")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("openStream did not return within 15s: connect is not time-bounded")
	}
}

func assertSeqs(t *testing.T, got []Event, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("delivered %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, seq := range want {
		if got[i].Seq != seq {
			t.Fatalf("event %d has seq %d, want %d: %+v", i, got[i].Seq, seq, got)
		}
	}
}
