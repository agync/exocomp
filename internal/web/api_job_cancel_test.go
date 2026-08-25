package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestAPIJobCancel covers the job-cancel write: unauthenticated is 401, a
// non-operator scope is forbidden (operator-only, the write floor), and an
// operator cancels a parked job — its resulting state comes back cancelled.
func TestAPIJobCancel(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	// A gated job sits in awaiting_approval — non-terminal and unclaimed, so it
	// is stably cancellable, and the transition it takes (awaiting_approval ->
	// cancelled) is the exact one the reactive gate uses on a rejection.
	job, _ := h.gatedJob(t, time.Hour)
	path := "/api/v1/jobs/" + itoa64(job.ID) + "/cancel"

	// (a) unauthenticated -> 401
	if code, _, _ := bearerPost(t, ts.URL, path, "", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated cancel status = %d, want 401", code)
	}

	// (b) wrong scope: the approve scope may decide, but may not cancel.
	ap := h.mint(t, "approver", "approve")
	code, _, body := bearerPost(t, ts.URL, path, ap, "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("approve-scope cancel status = %d, want 403 (%s)", code, body)
	}
	parked, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if parked.State != store.StateAwaitingApproval {
		t.Fatalf("job state = %q, want awaiting_approval — a non-operator cancelled it", parked.State)
	}

	// (c) happy path: an operator cancels it, and the response carries the
	// resulting state.
	op := h.mint(t, "op", "operator")
	code, _, body = bearerPost(t, ts.URL, path, op, "", nil)
	if code != http.StatusOK {
		t.Fatalf("operator cancel status = %d, want 200 (%s)", code, body)
	}
	var jv struct {
		State string `json:"state"`
	}
	decodeData(t, body, &jv)
	if jv.State != string(store.StateCancelled) {
		t.Fatalf("resulting state = %q, want cancelled (%s)", jv.State, body)
	}
	got, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.State != store.StateCancelled {
		t.Fatalf("stored job state = %q, want cancelled", got.State)
	}

	// A second cancel of the now-terminal job is a 409, never a silent success.
	code, _, body = bearerPost(t, ts.URL, path, op, "", nil)
	if code != http.StatusConflict {
		t.Fatalf("re-cancel status = %d, want 409 (%s)", code, body)
	}
}

// TestAPIJobCancelUnknown proves an unknown job id is a 404.
func TestAPIJobCancelUnknown(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	op := h.mint(t, "op", "operator")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/jobs/999999/cancel", op, "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown-job cancel status = %d, want 404 (%s)", code, body)
	}
}
