package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// maxApprovalStreams and refuses the next with 503. With the per-identity
// sub-cap of 2, four distinct sessions are needed to fill the total of 8.
func TestApprovalsStreamCap(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	open := make([]*http.Response, 0, maxApprovalStreams)
	defer func() {
		for _, r := range open {
			_ = r.Body.Close()
		}
	}()
	for i := 0; i < maxApprovalStreams/maxApprovalStreamsPerToken; i++ {
		client := login(t, ts.URL, h.mint(t, fmt.Sprintf("op%d", i), "operator"))
		for j := 0; j < maxApprovalStreamsPerToken; j++ {
			resp, err := client.Get(ts.URL + "/approvals/stream")
			if err != nil {
				t.Fatalf("open stream %d/%d: %v", i, j, err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("stream %d/%d status = %d, want 200", i, j, resp.StatusCode)
			}
			open = append(open, resp)
		}
	}

	// A 9th stream from any identity hits the total cap.
	over, err := login(t, ts.URL, h.mint(t, "over", "operator")).Get(ts.URL + "/approvals/stream")
	if err != nil {
		t.Fatalf("over-cap stream: %v", err)
	}
	defer func() { _ = over.Body.Close() }()
	if over.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap stream status = %d, want 503", over.StatusCode)
	}
}

// TestApprovalsStreamPerTokenCap is the F3 per-identity sub-cap: one token id
// may hold at most maxApprovalStreamsPerToken streams, so a single identity
// cannot fill every slot. The third stream from the same session is refused
// with 503, and closing one of the token's streams frees the slot again.
func TestApprovalsStreamPerTokenCap(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	open := make([]*http.Response, 0, maxApprovalStreamsPerToken)
	defer func() {
		for _, r := range open {
			_ = r.Body.Close()
		}
	}()
	for i := 0; i < maxApprovalStreamsPerToken; i++ {
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
		t.Fatalf("over per-token stream: %v", err)
	}
	defer func() { _ = over.Body.Close() }()
	if over.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over per-token stream status = %d, want 503", over.StatusCode)
	}

	if err := open[0].Body.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
	open = open[1:]
	again, err := client.Get(ts.URL + "/approvals/stream")
	if err != nil {
		t.Fatalf("re-open after close: %v", err)
	}
	defer func() { _ = again.Body.Close() }()
	if again.StatusCode != http.StatusOK {
		t.Fatalf("re-open after close status = %d, want 200", again.StatusCode)
	}
}

// TestApprovalsStreamPerTokenDoesNotBlockOtherIdentity: the per-token sub-cap
// must not let one identity starve another — a readonly session holding its two
// slots leaves an operator session's stream open (the total cap still applies,
// but identities are accounted separately).
func TestApprovalsStreamPerTokenDoesNotBlockOtherIdentity(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))
	roStreams := make([]*http.Response, 0, maxApprovalStreamsPerToken)
	defer func() {
		for _, r := range roStreams {
			_ = r.Body.Close()
		}
	}()
	for i := 0; i < maxApprovalStreamsPerToken; i++ {
		resp, err := ro.Get(ts.URL + "/approvals/stream")
		if err != nil {
			t.Fatalf("readonly stream %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("readonly stream %d status = %d, want 200", i, resp.StatusCode)
		}
		roStreams = append(roStreams, resp)
	}

	op := login(t, ts.URL, h.mint(t, "op", "operator"))
	resp, err := op.Get(ts.URL + "/approvals/stream")
	if err != nil {
		t.Fatalf("operator stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("operator stream status = %d, want 200 (readonly must not block it)", resp.StatusCode)
	}
}

// TestApprovalsStreamAPIRefusalIsWireEnvelope: an over-cap refusal on the API
// face answers with the wire error envelope and a Retry-After, so a machine
// client backs off instead of seeing a bare 503 body it cannot parse.
func TestApprovalsStreamAPIRefusalIsWireEnvelope(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	token := h.mint(t, "agent", "readonly")
	open := make([]*http.Response, 0, maxApprovalStreamsPerToken)
	defer func() {
		for _, r := range open {
			_ = r.Body.Close()
		}
	}()
	for i := 0; i < maxApprovalStreamsPerToken; i++ {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/approvals/stream", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d status = %d, want 200", i, resp.StatusCode)
		}
		open = append(open, resp)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/approvals/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	over, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("over-cap stream: %v", err)
	}
	defer func() { _ = over.Body.Close() }()
	if over.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap stream status = %d, want 503", over.StatusCode)
	}
	if ra := over.Header.Get("Retry-After"); ra == "" {
		t.Fatal("over-cap stream refusal carries no Retry-After")
	}
	var env struct {
		Error wire.Error `json:"error"`
	}
	body, _ := io.ReadAll(over.Body)
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("over-cap stream refusal is not a wire envelope: %v (%s)", err, body)
	}
	if env.Error.Code != wire.CodeKDFBusy {
		t.Fatalf("over-cap stream refusal code = %q, want %q", env.Error.Code, wire.CodeKDFBusy)
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

// TestApprovalsStreamHeartbeat is the F2 regression: an idle approvals stream
// used to write nothing for hours, so any intermediary idle timeout killed the
// live list silently. The tail now emits an SSE comment heartbeat (: keepalive)
// on its idle timer.
func TestApprovalsStreamHeartbeat(t *testing.T) {
	h := newHarness(t)
	h.server.heartbeat = 100 * time.Millisecond
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp, err := client.Get(ts.URL + "/approvals/stream")
	if err != nil {
		t.Fatalf("GET approvals stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approvals stream status = %d, want 200", resp.StatusCode)
	}

	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		scan := bufio.NewScanner(resp.Body)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before the heartbeat arrived")
			}
			if line == ": keepalive" {
				return
			}
		case <-deadline:
			t.Fatal("no heartbeat written during an idle window")
		}
	}
}

// TestApprovalsStreamEndsOnStop is the F5 regression: exocomp web used to let
// an attached approvals stream hold the listener open, so a SIGINT burned the
// whole grace period. Stopping the surface closes streamStop, which ends the
// tail on its next select.
func TestApprovalsStreamEndsOnStop(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp, err := client.Get(ts.URL + "/approvals/stream")
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
	waitApprovalsEvent(t, events, 3*time.Second)

	h.server.StopStreams()

	select {
	case views, ok := <-events:
		if ok {
			t.Fatalf("stream delivered an event after StopStreams: %+v", views)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream stayed open after StopStreams")
	}
}

// TestApprovalsStreamMatchingFPSuppressesSnapshot is the F4 regression: a page
// that already rendered the pending list opens the stream with ?fp= set to that
// state's fingerprint, so the connect-time snapshot would only reload a list
// that is already current. A matching fp must stay silent until the first real
// change.
func TestApprovalsStreamMatchingFPSuppressesSnapshot(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	rows, err := h.store.ListApprovals(context.Background(), true)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	fp := approvalsFP(rows)

	resp, err := client.Get(ts.URL + "/approvals/stream?fp=" + fp)
	if err != nil {
		t.Fatalf("GET approvals stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approvals stream status = %d, want 200", resp.StatusCode)
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

	select {
	case views, ok := <-events:
		t.Fatalf("matching fp delivered an initial snapshot %+v (open=%v)", views, ok)
	case <-time.After(800 * time.Millisecond):
	}

	// A real change still streams.
	_, approval := h.gatedJob(t, time.Hour)
	second := waitApprovalsEvent(t, events, 3*time.Second)
	if len(second) != 1 || second[0].ID != approval.ID {
		t.Fatalf("post-create event = %+v, want exactly one approval with id %d", second, approval.ID)
	}
}

// TestApprovalsStreamDifferingFPSendsSnapshot is the F4 counterpart: a stale
// ?fp= means the page rendered a different state than the stream's, so the
// connect-time snapshot must be sent immediately or the render-to-connect
// change is silently dropped. An absent fp (the API route) keeps the same
// behavior, which is what TestApprovalsStreamInitialSnapshotThenChange guards.
func TestApprovalsStreamDifferingFPSendsSnapshot(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp, err := client.Get(ts.URL + "/approvals/stream?fp=deadbeef")
	if err != nil {
		t.Fatalf("GET approvals stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approvals stream status = %d, want 200", resp.StatusCode)
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
}

// TestApprovalsStreamEndsOnBearerRotate is the F1 bearer half: the approvals
// tail re-authenticates the row per poll, but a rotated token's row stays
// Active, so a machine-held stream on the OLD secret used to keep streaming up
// to the lifetime cap. The tail pins the credential epoch at attach, so a
// rotate ends the stream on the next poll.
func TestApprovalsStreamEndsOnBearerRotate(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

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

	if _, _, err := RotateToken(ctx, h.store, "agent", 0, false); err != nil {
		t.Fatalf("RotateToken: %v", err)
	}

	select {
	case views, ok := <-events:
		if ok {
			t.Fatalf("stream delivered an event after bearer rotate: %+v", views)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream stayed open after bearer rotate")
	}
}

// TestApprovalsFingerprintReflectsRenderedRows is the F7 regression: the
// approvals page's ?fp= must match the exact rows the page renders. The fp and
// the render used to come from separate ListApprovals calls, so a change
// between them left a page whose fingerprint promised a state its rows did not
// show; the handler now computes both from one fetch, so the fp the stream
// compares is provably the state on screen.
func TestApprovalsFingerprintReflectsRenderedRows(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, approval := h.gatedJob(t, time.Hour)

	code, page := getBody(t, client, ts.URL+"/approvals")
	if code != 200 {
		t.Fatalf("approvals page status = %d", code)
	}
	// The rendered table names the seeded approval...
	if !strings.Contains(page, "/approvals/"+itoa64(approval.ID)) {
		t.Fatalf("approvals page did not render approval #%d", approval.ID)
	}
	// ...and the fingerprint is computed over the same ListApprovals rows that
	// produced that table.
	rows, err := h.store.ListApprovals(context.Background(), true)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	wantFP := approvalsFP(rows)
	if !strings.Contains(page, `data-fp="`+wantFP+`"`) {
		t.Fatalf("approvals page fingerprint does not match the rendered rows (want %q)", wantFP)
	}
}

// TestApprovalsPageLiveScript asserts the /approvals page renders the
// approvalsLive script with the data-fp fingerprint and no seenFirst blind skip:
// it opens the browser stream with ?fp= set to the rendered state's
// fingerprint, reloads on EVERY approvals event (the server already skips
// the reload-only snapshot when the fingerprints match), and never closes on
// error (EventSource reconnects on its own). Grepping the rendered script is the
// regression check that the reload-loop fix is actually in the shipped markup, not
// just in a template that never renders.
func TestApprovalsPageLiveScript(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/approvals")
	if code != 200 {
		t.Fatalf("approvals page status = %d", code)
	}
	if !strings.Contains(page, `data-fp="`) {
		t.Fatal("approvals page renders the approvalsLive script without the data-fp fingerprint")
	}
	if !strings.Contains(page, `new EventSource("/approvals/stream?fp="`) {
		t.Fatal("approvals page does not open the browser stream with the ?fp= fingerprint")
	}
	if strings.Contains(page, "seenFirst") {
		t.Fatal("approvals page still renders the seenFirst blind skip")
	}
	if strings.Contains(page, "es.close()") {
		t.Fatal("approvals page still closes the stream on error instead of letting EventSource reconnect")
	}
	if !strings.Contains(page, "location.reload()") {
		t.Fatal("approvals page does not reload on an approvals event")
	}
	if !strings.Contains(page, `id="approvals-stale"`) {
		t.Fatal("approvals page missing the stale-live-updates banner element")
	}
	if !strings.Contains(page, "es.onerror") {
		t.Fatal("approvals page does not surface a refused/ended stream via the stale banner")
	}
}
