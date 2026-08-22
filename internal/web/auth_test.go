package web

import (
	"context"
	"testing"
	"time"
)

func TestIsLoopbackBind(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8443":    true,
		"localhost:8443":    true,
		"[::1]:8443":        true,
		"0.0.0.0:8443":      false,
		":8443":             false,
		"192.168.1.10:8443": false,
		"[::]:8443":         false,
	}
	for addr, want := range cases {
		if got := isLoopbackBind(addr); got != want {
			t.Errorf("isLoopbackBind(%q) = %v, want %v", addr, got, want)
		}
	}
}

// TestRequireTokenForBind is the ADR 0009 point-1 gate: a non-loopback bind is
// refused without a token, allowed with one, and --insecure-http is loud.
func TestRequireTokenForBind(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Loopback never needs a token.
	if _, err := RequireTokenForBind(ctx, h.store, "127.0.0.1:8443", false, now); err != nil {
		t.Fatalf("loopback bind refused: %v", err)
	}

	// A LAN bind with no active token is refused.
	if _, err := RequireTokenForBind(ctx, h.store, "0.0.0.0:8443", false, now); err == nil {
		t.Fatal("a non-loopback bind with no token must be refused")
	}

	// Once a token exists, the LAN bind is allowed and TLS is silent.
	h.mint(t, "laptop", "operator")
	warn, err := RequireTokenForBind(ctx, h.store, "0.0.0.0:8443", false, now)
	if err != nil {
		t.Fatalf("LAN bind with a token refused: %v", err)
	}
	if warn != "" {
		t.Fatalf("a TLS LAN bind should be silent, got warning: %q", warn)
	}

	// --insecure-http warns, loudly, even with a token.
	warn, err = RequireTokenForBind(ctx, h.store, "0.0.0.0:8443", true, now)
	if err != nil {
		t.Fatalf("insecure LAN bind refused: %v", err)
	}
	if warn == "" {
		t.Fatal("--insecure-http must warn")
	}
}

func TestSessionResolvesAndExpires(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	token := h.mint(t, "laptop", "operator")

	row, err := h.server.authenticateToken(ctx, token, time.Now().UTC())
	if err != nil {
		t.Fatalf("authenticate minted token: %v", err)
	}

	now := time.Now().UTC()
	value, _ := h.server.mintSession(row, now)
	id, err := h.server.resolveSession(value, now)
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if id != row.ID {
		t.Fatalf("session resolved to token %d, want %d", id, row.ID)
	}

	// A tampered signature never resolves.
	if _, err := h.server.resolveSession(value+"x", now); err == nil {
		t.Fatal("a tampered session cookie must not resolve")
	}
	// An expired cookie never resolves.
	if _, err := h.server.resolveSession(value, now.Add(2*sessionTTL)); err == nil {
		t.Fatal("an expired session cookie must not resolve")
	}
}
