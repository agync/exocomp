package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/wire"
)

// TestApprovalsStreamInitialSnapshotThenChange is the fix-2 regression test:
// the phone polled GET /api/v1/approvals for this; this proves the SSE
// replacement delivers the same information without polling — one snapshot
// on connect, even with nothing pending, then exactly one more event when a
// new pending approval appears.
func TestApprovalsStreamInitialSnapshotThenChange(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	token := h.mint(t, "agent", "readonly")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/approvals/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET approvals stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approvals stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("approvals stream content-type = %q, want text/event-stream", ct)
	}

	events := make(chan []wire.ApprovalView, 8)
	go func() {
		defer close(events)
		scan := bufio.NewScanner(resp.Body)
		for scan.Scan() {
			line := scan.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var views []wire.ApprovalView
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &views); err != nil {
				continue
			}
			events <- views
		}
	}()

	first := waitApprovalsEvent(t, events, 3*time.Second)
	if len(first) != 0 {
		t.Fatalf("initial snapshot = %d approvals, want 0 (empty store)", len(first))
	}

	_, approval := h.gatedJob(t, time.Hour)

	second := waitApprovalsEvent(t, events, 3*time.Second)
	if len(second) != 1 || second[0].ID != approval.ID {
		t.Fatalf("post-create event = %+v, want exactly one approval with id %d", second, approval.ID)
	}
}

// waitApprovalsEvent reads the next approvals-event payload off events,
// failing the test if the stream closes or nothing arrives before the
// deadline.
func waitApprovalsEvent(t *testing.T, events <-chan []wire.ApprovalView, within time.Duration) []wire.ApprovalView {
	t.Helper()
	select {
	case views, ok := <-events:
		if !ok {
			t.Fatal("approvals stream closed before delivering an event")
		}
		return views
	case <-time.After(within):
		t.Fatal("approvals stream never delivered an event")
		return nil
	}
}

// TestApprovalsStreamBrowser mounts the browser session at /approvals/stream
// (cookie-auth, not bearer) and proves it streams: a snapshot on connect, then
// exactly one more event when a new pending approval appears.
func TestApprovalsStreamBrowser(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/approvals/stream", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET approvals stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approvals stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("approvals stream content-type = %q, want text/event-stream", ct)
	}

	events := make(chan []wire.ApprovalView, 8)
	go func() {
		defer close(events)
		scan := bufio.NewScanner(resp.Body)
		for scan.Scan() {
			line := scan.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var views []wire.ApprovalView
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &views); err != nil {
				continue
			}
			events <- views
		}
	}()

	first := waitApprovalsEvent(t, events, 3*time.Second)
	if len(first) != 0 {
		t.Fatalf("initial snapshot = %d approvals, want 0 (empty store)", len(first))
	}

	_, approval := h.gatedJob(t, time.Hour)

	second := waitApprovalsEvent(t, events, 3*time.Second)
	if len(second) != 1 || second[0].ID != approval.ID {
		t.Fatalf("post-create event = %+v, want exactly one approval with id %d", second, approval.ID)
	}
}

// TestApprovalsStreamEndsOnSessionRotate is the F1 regression: the browser
// stream used to re-authenticate only the token row, so a rotated token — whose
// row stays Active — kept an already-attached stream alive up to the lifetime
// cap. The session is now re-resolved per poll, and resolve() enforces the
// credential-epoch pin, so a rotate ends the stream on the next poll.
func TestApprovalsStreamEndsOnSessionRotate(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/approvals/stream", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET approvals stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	events := make(chan []wire.ApprovalView, 8)
	go func() {
		defer close(events)
		scan := bufio.NewScanner(resp.Body)
		for scan.Scan() {
			line := scan.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var views []wire.ApprovalView
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &views); err != nil {
				continue
			}
			events <- views
		}
	}()
	// The stream is attached once its initial snapshot arrives.
	waitApprovalsEvent(t, events, 3*time.Second)

	if _, _, err := RotateToken(ctx, h.store, "laptop", 0, false); err != nil {
		t.Fatalf("RotateToken: %v", err)
	}
	// The rotation dropped the session outright.
	if code := getNoRedirect(t, client, ts.URL+"/"); code == http.StatusOK {
		t.Fatal("session still authenticated after rotate")
	}

	// The attached stream ends on the next poll.
	select {
	case views, ok := <-events:
		if ok {
			t.Fatalf("stream delivered an event after session rotate: %+v", views)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream stayed open after session rotate")
	}
}

// TestApprovalsStreamCap is the F3 regression: unbounded per-page pollers on
// the one-connection store pool. tailApprovals caps concurrent streams at
// maxApprovalStreams and refuses the next with 503.
func TestApprovalsStreamCap(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	open := make([]*http.Response, 0, maxApprovalStreams)
	defer func() {
		for _, r := range open {
			_ = r.Body.Close()
		}
	}()
	for i := 0; i < maxApprovalStreams; i++ {
		resp, err := client.Get(ts.URL + "/approvals/stream")
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d status = %d, want 200", i, resp.StatusCode)
		}
		open = append(open, resp)
	}

	over, err := client.Get(ts.URL + "/approvals/stream")
	if err != nil {
		t.Fatalf("over-cap stream: %v", err)
	}
	defer func() { _ = over.Body.Close() }()
	if over.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap stream status = %d, want 503", over.StatusCode)
	}
}

// TestApprovalsStreamPollInterval is the F3 regression on cadence: the
// approvals tail must not reuse the 300ms job-tail poll on the shared store
// pool.
func TestApprovalsStreamPollInterval(t *testing.T) {
	if approvalsStreamPoll != 2*time.Second {
		t.Fatalf("approvalsStreamPoll = %v, want 2s", approvalsStreamPoll)
	}
	if approvalsStreamPoll == streamPoll {
		t.Fatal("approvals stream must not share the job tail's 300ms poll")
	}
}

// TestApprovalsPageLiveScript asserts the /approvals page renders the
// approvalsLive script with the seenFirst guard: it opens the browser stream
// and ignores the FIRST approvals event (the initial snapshot this page
// already rendered), reloading only on a SUBSEQUENT change. Grepping the
// rendered script for the guard is the regression check that the reload-loop
// fix is actually in the shipped markup, not just in a template that never
// renders.
func TestApprovalsPageLiveScript(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/approvals")
	if code != 200 {
		t.Fatalf("approvals page status = %d", code)
	}
	if !strings.Contains(page, `new EventSource("/approvals/stream")`) {
		t.Fatal("approvals page does not open the browser stream")
	}
	if !strings.Contains(page, "seenFirst") ||
		!strings.Contains(page, "if (!seenFirst) { seenFirst = true; return; }") ||
		!strings.Contains(page, "location.reload()") {
		t.Fatal("approvals page renders the approvalsLive script without the seenFirst guard")
	}
}
