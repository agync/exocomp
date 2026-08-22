package main

import (
	"context"
	"io"
	"strings"
	"testing"
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
