package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/web"
)

// TestServeWebSurfaceSeparateFromLoopbackBackend proves item-7: the state
// backend binds loopback-only and takes no LAN token, while the web+API surface
// is a separate non-loopback listener that refuses to bind without a token and,
// once one exists, serves — gated — to a caller presenting it (Decision 0009).
func TestServeWebSurfaceSeparateFromLoopbackBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	dir := initForSource(t)
	ctx := context.Background()

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()

	// The state backend binds loopback-only.
	backend, err := api.New(api.Options{Store: st, Tokens: env, Port: 0})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	if err := backend.Start(); err != nil {
		t.Fatalf("backend start: %v", err)
	}
	defer func() { _ = backend.Shutdown(ctx) }()
	if !strings.HasPrefix(backend.Addr(), "127.0.0.1:") {
		t.Fatalf("state backend addr = %q, want a 127.0.0.1 bind", backend.Addr())
	}

	engine, err := webEngine(st, env)
	if err != nil {
		t.Fatalf("web engine: %v", err)
	}

	// A non-loopback web bind with no active token refuses (Decision 0009 gate).
	if _, err := startWebSurface(ctx, st, env, dir, engine, nil, nil, "", "0.0.0.0:0", true, io.Discard); err == nil {
		t.Fatal("expected a non-loopback web bind with no token to refuse")
	}

	// With a token minted, the LAN surface binds and serves.
	token, _, err := web.MintToken(ctx, st, "laptop", "operator", 0)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	surface, err := startWebSurface(ctx, st, env, dir, engine, nil, nil, "", "0.0.0.0:0", true, io.Discard)
	if err != nil {
		t.Fatalf("start web surface: %v", err)
	}
	defer func() {
		shCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_ = surface.Shutdown(shCtx)
	}()

	_, port, err := net.SplitHostPort(surface.addr)
	if err != nil {
		t.Fatalf("split surface addr %q: %v", surface.addr, err)
	}
	base := "http://127.0.0.1:" + port
	client := &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// Unauthenticated dashboard is gated: redirected to the login form.
	resp, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauth GET / status = %d, want 303 (gated)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/login") {
		t.Errorf("unauth redirect Location = %q, want /login", loc)
	}

	// The same request with the LAN token authenticates and serves.
	req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	authed, err := client.Do(req)
	if err != nil {
		t.Fatalf("authed GET /: %v", err)
	}
	defer func() { _ = authed.Body.Close() }()
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("authed GET / status = %d, want 200", authed.StatusCode)
	}
	body, _ := io.ReadAll(authed.Body)
	if !strings.Contains(string(body), "<main>") {
		t.Errorf("authed dashboard did not render the main content")
	}
}
