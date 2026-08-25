package web

import (
	"bufio"
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
