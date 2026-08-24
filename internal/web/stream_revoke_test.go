package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
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
