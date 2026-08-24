package main

import (
	"strings"
	"testing"
)

func TestTokenRotateRevivesAndReseals(t *testing.T) {
	initForToken(t)

	first, _, err := runToken(t, "new", "abn", "--scope", "operator")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if !strings.Contains(first, "exo_") {
		t.Fatalf("new printed no token:\n%s", first)
	}

	if _, _, err := runToken(t, "revoke", "abn"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// A revoked name stays reserved; new must explain and point at rotate.
	_, _, err = runToken(t, "new", "abn", "--scope", "operator")
	if err == nil {
		t.Fatal("re-minting a revoked name must fail")
	}
	for _, want := range []string{"revoked", "rotate", "list --all"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("revoked-collision error missing %q: %v", want, err)
		}
	}

	// Rotate revives the name with a fresh secret.
	out, _, err := runToken(t, "rotate", "abn")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !strings.Contains(out, "rotated") || !strings.Contains(out, "exo_") {
		t.Fatalf("rotate output unexpected:\n%s", out)
	}

	// The token is active again and listed by default.
	list, _, err := runToken(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, "abn") || !strings.Contains(list, "active") {
		t.Fatalf("rotated token not active in list:\n%s", list)
	}
}

func TestTokenNewActiveCollisionSuggestsRotate(t *testing.T) {
	initForToken(t)
	if _, _, err := runToken(t, "new", "laptop", "--scope", "operator"); err != nil {
		t.Fatalf("new: %v", err)
	}
	_, _, err := runToken(t, "new", "laptop", "--scope", "operator")
	if err == nil {
		t.Fatal("a duplicate active name must fail")
	}
	if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "rotate") {
		t.Fatalf("active-collision error missing hint: %v", err)
	}
}

func TestTokenRotateMissing(t *testing.T) {
	initForToken(t)
	_, _, err := runToken(t, "rotate", "ghost")
	if err == nil || !strings.Contains(err.Error(), "no token named") {
		t.Fatalf("rotate missing = %v, want a 'no token named' error", err)
	}
}
