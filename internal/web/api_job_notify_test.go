package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// TestAPIJobNotifyRegisters covers the one-shot notify write: unauthenticated is
// 401, a non-operator scope is forbidden, an operator registers a durable
// request against a non-terminal job (recorded once, idempotent on a repeat),
// and an unknown id is a 404.
func TestAPIJobNotifyRegisters(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// A gated job sits in awaiting_approval — non-terminal, and never claimed by
	// the running queue, so it stays a stable target.
	job, _ := h.gatedJob(t, time.Hour)
	path := "/api/v1/jobs/" + itoa64(job.ID) + "/notify"

	// (a) unauthenticated -> 401
	if code, _, _ := bearerPost(t, ts.URL, path, "", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated notify status = %d, want 401", code)
	}

	// (b) wrong scope -> 403
	ro := h.mint(t, "ro", "readonly")
	if code, _, rb := bearerPost(t, ts.URL, path, ro, "", nil); code != http.StatusForbidden {
		t.Fatalf("readonly notify status = %d, want 403 (%s)", code, rb)
	}

	// (c) happy path -> 200, and one durable request row is recorded.
	op := h.mint(t, "op", "operator")
	code, _, rb := bearerPost(t, ts.URL, path, op, "", nil)
	if code != http.StatusOK {
		t.Fatalf("operator notify status = %d, want 200 (%s)", code, rb)
	}
	kind, _, _ := decodeEnvelope(t, rb)
	if kind != kindJobNotify {
		t.Fatalf("notify kind = %q, want %q (%s)", kind, kindJobNotify, rb)
	}
	var v jobNotifyView
	decodeData(t, rb, &v)
	if v.JobID != job.ID || !v.Registered {
		t.Fatalf("notify view = %+v, want job %d registered", v, job.ID)
	}
	if n := countNotifyRequests(t, h, job.ID); n != 1 {
		t.Fatalf("recorded %d notify requests, want 1", n)
	}

	// (d) idempotent: a second request records no second row.
	if code, _, _ := bearerPost(t, ts.URL, path, op, "", nil); code != http.StatusOK {
		t.Fatalf("second notify status = %d, want 200", code)
	}
	if n := countNotifyRequests(t, h, job.ID); n != 1 {
		t.Fatalf("recorded %d notify requests after a repeat, want 1", n)
	}

	// (e) unknown id -> 404
	if code, _, _ := bearerPost(t, ts.URL, "/api/v1/jobs/999999/notify", op, "", nil); code != http.StatusNotFound {
		t.Fatalf("unknown-job notify status = %d, want 404", code)
	}
}

// TestAPIJobNotifyTerminalIsConflict: arming a page on a job that already
// reached a terminal state is a 409 — there is nothing to wait for.
func TestAPIJobNotifyTerminalIsConflict(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// A plain shell job the running queue claims and finishes.
	op := h.mint(t, "op", "operator")
	done := h.waitJobState(t, h.enqueueShell(t), store.StateSucceeded, 5*time.Second)

	code, _, rb := bearerPost(t, ts.URL, "/api/v1/jobs/"+itoa64(done.ID)+"/notify", op, "", nil)
	if code != http.StatusConflict {
		t.Fatalf("terminal notify status = %d, want 409 (%s)", code, rb)
	}
	if n := countNotifyRequests(t, h, done.ID); n != 0 {
		t.Fatalf("a terminal job recorded %d notify requests, want 0", n)
	}
}

// enqueueShell enqueues a trivial shell job the running queue will finish.
func (h *harness) enqueueShell(t *testing.T) int64 {
	t.Helper()
	job, err := h.queue.Enqueue(context.Background(), queue.Spec{
		Runbook: "noop", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return job.ID
}

func countNotifyRequests(t *testing.T, h *harness, jobID int64) int {
	t.Helper()
	n, err := h.store.CountEventsByDedupKey(context.Background(), reactive.NotifyRequestDedupKey(jobID))
	if err != nil {
		t.Fatalf("CountEventsByDedupKey: %v", err)
	}
	return n
}
