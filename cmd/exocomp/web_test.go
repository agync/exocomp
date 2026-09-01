package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/web"
)

// TestWebRefusesTokenlessLANBind is the ADR 0009 gate at the command boundary:
// `exocomp web` on a non-loopback address with no token refuses before it ever
// binds.
func TestWebRefusesTokenlessLANBind(t *testing.T) {
	initForToken(t) // initialized data dir, but no tokens minted

	err := runWeb(context.Background(), io.Discard, io.Discard,
		webOptions{addr: "0.0.0.0:0"})
	if err == nil {
		t.Fatal("a tokenless LAN bind must be refused")
	}
	if !strings.Contains(err.Error(), "no active token") {
		t.Fatalf("error = %v, want it to name the missing token", err)
	}
}

// TestWebLoopbackNeedsNoToken: a loopback bind is always allowed. We start it
// and cancel immediately, asserting it does not refuse for lack of a token.
func TestWebLoopbackAllowed(t *testing.T) {
	initForToken(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stop it as soon as it starts serving
	err := runWeb(ctx, io.Discard, io.Discard, webOptions{addr: "127.0.0.1:0"})
	if err != nil && strings.Contains(err.Error(), "no active token") {
		t.Fatalf("loopback bind was refused for lack of a token: %v", err)
	}
}

// TestWebShutdownReturnsNilWithStreamAttached is the F5 exit-code claim: a
// SIGINT while an approvals stream is attached must still shut the surface
// down cleanly (StopStreams ends the tails before http.Shutdown's grace clock
// starts), and runWeb must return nil — the old behaviour let an attached
// stream hold the listener open through the whole grace window.
func TestWebShutdownReturnsNilWithStreamAttached(t *testing.T) {
	dir := initForToken(t)

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	token, _, err := web.MintToken(context.Background(), st, "op", "operator", 0)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- runWeb(ctx, &out, io.Discard, webOptions{addr: "127.0.0.1:0", insecure: true}) }()

	addr := waitWebListen(t, &out)
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/approvals/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET approvals stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approvals stream status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWeb returned %v after SIGINT with a stream attached, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runWeb did not return after SIGINT with a stream attached")
	}
}

// waitWebListen polls runWeb's output for its listening line and returns the
// bound addr, so the caller can attach a stream before signalling shutdown.
func waitWebListen(t *testing.T, out *bytes.Buffer) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if s := out.String(); s != "" {
			for _, line := range strings.Split(s, "\n") {
				if rest, ok := strings.CutPrefix(line, "web UI listening on http://"); ok {
					return rest
				}
			}
		}
		select {
		case <-deadline:
			t.Fatalf("runWeb never printed its listening line; output so far: %q", out.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
