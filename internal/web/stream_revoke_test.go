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
	"sync/atomic"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// TestJobStreamEndsOnRevoke is a regression test: auth was resolved once, at
// attach, and the tail then polled forever — so a leaked token's holder kept
// reading a job's log across the revoke that was supposed to cut it off. The
// tail now re-resolves the credential as it polls and ends the stream when the
// row stops authenticating.
func TestJobStreamEndsOnRevoke(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	// A long-running job so the stream stays open on its own account.
	job, err := h.queue.EnqueueGated(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	token := h.mint(t, "leaked", "readonly")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa64(job.ID)+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	appendLine := func(msg string) {
		if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
			JobID: job.ID, Level: store.LevelInfo, Msg: msg,
		}); err != nil {
			t.Fatalf("AppendJobEvent: %v", err)
		}
	}

	// The stream is live before the revoke.
	appendLine("before-revoke")
	scan := bufio.NewScanner(resp.Body)
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()
	if !waitFor(lines, "before-revoke", 3*time.Second) {
		t.Fatal("stream never delivered the pre-revoke event")
	}

	if err := h.store.RevokeToken(ctx, "leaked"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	// A fresh request proves the revoke took effect immediately.
	if code, _, _ := bearerGet(t, ts.URL, "/api/v1/jobs", token); code != http.StatusUnauthorized {
		t.Fatalf("post-revoke read status = %d, want 401", code)
	}

	appendLine("after-revoke")
	if waitFor(lines, "after-revoke", 3*time.Second) {
		t.Fatal("the open stream kept delivering events after its token was revoked")
	}
}

// waitFor reports whether a line containing want arrives before the deadline. A
// closed channel (the stream ended) reports false.
func waitFor(lines <-chan string, want string, within time.Duration) bool {
	deadline := time.After(within)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return false
			}
			if strings.Contains(line, want) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// openNStreams opens n streams from the given GET func, asserting each is 200,
// and returns them for the caller to close. Used by the cap-reorder tests to
// fill (and refill) a token's share. A failed open closes the streams opened so
// far — a test that fails partway must not strand live SSE bodies and hang the
// httptest server's Close().
func openNStreams(t *testing.T, get func() (*http.Response, error), n int, what string) []*http.Response {
	t.Helper()
	open := make([]*http.Response, 0, n)
	for i := 0; i < n; i++ {
		resp, err := get()
		if err != nil {
			closeAll(open)
			t.Fatalf("open %s %d: %v", what, i, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			closeAll(open)
			t.Fatalf("open %s %d status = %d, want 200", what, i, resp.StatusCode)
		}
		open = append(open, resp)
	}
	return open
}

func closeAll(rs []*http.Response) {
	for _, r := range rs {
		_ = r.Body.Close()
	}
}

// waitStreamAtOrBelow polls the server's global stream counter until it is at
// or below want, so a test that frees slots by closing bodies does not race
// the handler's async cleanup on its next attach.
func waitStreamAtOrBelow(t *testing.T, counter *atomic.Int64, want int64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if counter.Load() <= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream counter did not drop to %d (still %d)", want, counter.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestJobStreamHeartbeat pins the MED fix: the job tail now emits an SSE
// comment heartbeat (: keepalive) on its idle timer, so a long quiet running
// job is not silently killed by an intermediary idle timeout — mirroring the
// approvals tail.
func TestJobStreamHeartbeat(t *testing.T) {
	h := newHarness(t)
	h.server.heartbeat = 100 * time.Millisecond
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// A gated job stays non-terminal, so the tail has no done-frame to end on
	// and would otherwise write nothing at all.
	job, err := h.queue.EnqueueGated(context.Background(), queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	client := login(t, ts.URL, h.mint(t, "op", "operator"))

	resp, err := client.Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
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
			t.Fatal("no job-tail heartbeat written during an idle window")
		}
	}
}

// TestJobStreamCapReorderUndoesPerToken pins the F3 cap reorder (MED): the job
// tail charges the per-token cap before the global slot, and a global-cap
// refusal must UNDO that per-token charge — otherwise a refused attach leaks a
// phantom per-token slot that silently shrinks that identity's share. Fill the
// global cap with two identities, refuse a third at the global level, free the
// slots, then the refused identity must still open its FULL per-token share.
// Reverting the reorder (charging per-token without the undo) makes the fourth
// open fail.
func TestJobStreamCapReorderUndoesPerToken(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	job, err := h.queue.EnqueueGated(context.Background(), queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	stream := func(c *http.Client) func() (*http.Response, error) {
		return func() (*http.Response, error) { return c.Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream") }
	}

	a := login(t, ts.URL, h.mint(t, "a", "operator"))
	b := login(t, ts.URL, h.mint(t, "b", "operator"))
	aOpen := openNStreams(t, stream(a), maxJobStreamsPerToken, "a job stream")
	defer closeAll(aOpen)
	bOpen := openNStreams(t, stream(b), maxJobStreamsPerToken, "b job stream")
	defer closeAll(bOpen)

	// A third identity hits the global cap and is refused.
	c := login(t, ts.URL, h.mint(t, "c", "operator"))
	over, err := stream(c)()
	if err != nil {
		t.Fatalf("over-cap stream: %v", err)
	}
	_ = over.Body.Close()
	if over.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third-token stream status = %d, want 503", over.StatusCode)
	}

	// Free a's slots; the refused c must then open its full per-token share —
	// no phantom per-token charge survives the global refusal.
	closeAll(aOpen)
	waitStreamAtOrBelow(t, &h.server.jobStreams, maxJobStreams-maxJobStreamsPerToken, 3*time.Second)
	cOpen := openNStreams(t, stream(c), maxJobStreamsPerToken, "c job stream after freeing")
	defer closeAll(cOpen)
}

// TestJobStreamEndsOnBearerRotate is the F1/F4 tail: the job tail pins the
// credential epoch at attach, so a rotated bearer token — whose row stays
// Active — ends the tail on the next poll instead of streaming until the
// lifetime cap.
func TestJobStreamEndsOnBearerRotate(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	// A gated job stays awaiting_approval, so the tail has no terminal state to
	// end on its own and stays open for the rotate to cut off.
	job, err := h.queue.EnqueueGated(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	token := h.mint(t, "leaked", "readonly")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa64(job.ID)+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	appendLine := func(msg string) {
		if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
			JobID: job.ID, Level: store.LevelInfo, Msg: msg,
		}); err != nil {
			t.Fatalf("AppendJobEvent: %v", err)
		}
	}
	appendLine("before-rotate")
	scan := bufio.NewScanner(resp.Body)
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()
	if !waitFor(lines, "before-rotate", 3*time.Second) {
		t.Fatal("stream never delivered the pre-rotate event")
	}

	if _, _, err := RotateToken(ctx, h.store, "leaked", 0, false); err != nil {
		t.Fatalf("RotateToken: %v", err)
	}

	appendLine("after-rotate")
	if waitFor(lines, "after-rotate", 3*time.Second) {
		t.Fatal("the open job tail kept delivering events after its bearer token was rotated")
	}
}

// TestJobStreamEndsOnStop is the F4 half of the F5 shutdown fix: tailJobEvents
// now listens for streamStop, so `exocomp web`'s Shutdown ends attached job
// tails too — a SIGINT must not burn the grace window on a job tail any more
// than on an approvals tail.
func TestJobStreamEndsOnStop(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	job, err := h.queue.EnqueueGated(context.Background(), queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	client := login(t, ts.URL, h.mint(t, "op", "operator"))

	resp, err := client.Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	scan := bufio.NewScanner(resp.Body)
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()

	h.server.StopStreams()

	select {
	case _, ok := <-lines:
		if ok {
			t.Fatal("job tail delivered a line after StopStreams")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("job tail stayed open after StopStreams")
	}
}

// TestJobStreamEndsOnSessionRotate is the F5 session half of the rotate fix:
// a browser job tail is held by a session, not just a token row, so it must
// re-resolve the session per poll like tailApprovals — a rotated token ends the
// tail on the next poll instead of streaming until the lifetime cap.
func TestJobStreamEndsOnSessionRotate(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	job, err := h.queue.EnqueueGated(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp, err := client.Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	appendLine := func(msg string) {
		if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
			JobID: job.ID, Level: store.LevelInfo, Msg: msg,
		}); err != nil {
			t.Fatalf("AppendJobEvent: %v", err)
		}
	}
	appendLine("before-rotate")
	scan := bufio.NewScanner(resp.Body)
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()
	if !waitFor(lines, "before-rotate", 3*time.Second) {
		t.Fatal("stream never delivered the pre-rotate event")
	}

	if _, _, err := RotateToken(ctx, h.store, "laptop", 0, false); err != nil {
		t.Fatalf("RotateToken: %v", err)
	}

	appendLine("after-rotate")
	if waitFor(lines, "after-rotate", 3*time.Second) {
		t.Fatal("the open job tail kept delivering events after its session was rotated")
	}
}

// TestJobStreamPerTokenCap is the F5 regression: the shared job-tail funnel
// caps concurrent tails per token id, so one identity cannot hold every slot.
// The fifth tail from the same session is refused with 503, and closing one of
// the token's tails frees the slot again.
func TestJobStreamPerTokenCap(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	job, err := h.queue.EnqueueGated(context.Background(), queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	open := make([]*http.Response, 0, maxJobStreamsPerToken)
	defer func() {
		for _, r := range open {
			_ = r.Body.Close()
		}
	}()
	for i := 0; i < maxJobStreamsPerToken; i++ {
		resp, err := client.Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream")
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d status = %d, want 200", i, resp.StatusCode)
		}
		open = append(open, resp)
	}

	over, err := client.Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream")
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
	again, err := client.Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream")
	if err != nil {
		t.Fatalf("re-open after close: %v", err)
	}
	defer func() { _ = again.Body.Close() }()
	if again.StatusCode != http.StatusOK {
		t.Fatalf("re-open after close status = %d, want 200", again.StatusCode)
	}
}

// TestJobStreamTotalCap is the F5 regression on the global half of the job-tail
// cap: when a fleet of distinct identities has already filled every slot, the
// next tail is refused with 503 — and the per-token charge made just before the
// global refusal must be undone, or a refused attach would leak a slot forever.
func TestJobStreamTotalCap(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	job, err := h.queue.EnqueueGated(context.Background(), queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}

	open := make([]*http.Response, 0, maxJobStreams)
	defer func() {
		for _, r := range open {
			_ = r.Body.Close()
		}
	}()
	for i := 0; i < maxJobStreams/maxJobStreamsPerToken; i++ {
		client := login(t, ts.URL, h.mint(t, fmt.Sprintf("op%d", i), "operator"))
		for j := 0; j < maxJobStreamsPerToken; j++ {
			resp, err := client.Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream")
			if err != nil {
				t.Fatalf("open stream %d/%d: %v", i, j, err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("stream %d/%d status = %d, want 200", i, j, resp.StatusCode)
			}
			open = append(open, resp)
		}
	}

	over, err := login(t, ts.URL, h.mint(t, "over", "operator")).Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream")
	if err != nil {
		t.Fatalf("over-cap stream: %v", err)
	}
	defer func() { _ = over.Body.Close() }()
	if over.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap stream status = %d, want 503", over.StatusCode)
	}
	// The tail sets its SSE headers only after the cap check (MED), so a
	// refused stream is a plain 503 an EventSource cannot mistake for a stream.
	if ct := over.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("over-cap refusal must not carry SSE headers, got Content-Type %q", ct)
	}
}

// TestJobStreamAPIRefusalIsWireEnvelope is the F5 job-tail half of the refusal
// contract: an over-cap tail on the API face answers with the wire error
// envelope and a Retry-After so `jobs follow --remote` treats it as the
// retryable busy it is (streams_over_cap → exit 7), not a bare 503 body.
func TestJobStreamAPIRefusalIsWireEnvelope(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	job, err := h.queue.EnqueueGated(context.Background(), queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	token := h.mint(t, "agent", "readonly")

	open := make([]*http.Response, 0, maxJobStreamsPerToken)
	defer func() {
		for _, r := range open {
			_ = r.Body.Close()
		}
	}()
	for i := 0; i < maxJobStreamsPerToken; i++ {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa64(job.ID)+"/stream", nil)
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

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa64(job.ID)+"/stream", nil)
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
	if env.Error.Code != wire.CodeStreamsOverCap {
		t.Fatalf("over-cap stream refusal code = %q, want %q", env.Error.Code, wire.CodeStreamsOverCap)
	}
}
