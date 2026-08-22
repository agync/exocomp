package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestKnownHostRoundTrip proves host keys are recorded, re-recording a type
// replaces rather than duplicates, and the rendered file is the deterministic
// known_hosts body a fleet run verifies against.
func TestKnownHostRoundTrip(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	if err := s.RecordKnownHost(ctx, store.KnownHost{
		Host: "node0", KeyType: "ssh-ed25519", Key: "AAAAC3NzaC1lZDI1NTE5AAAAoriginal",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.RecordKnownHost(ctx, store.KnownHost{
		Host: "node0", KeyType: "ssh-rsa", Key: "AAAAB3NzaC1yc2Ersa",
	}); err != nil {
		t.Fatalf("record rsa: %v", err)
	}

	// Re-recording the SAME key is idempotent, not an error.
	if err := s.RecordKnownHost(ctx, store.KnownHost{
		Host: "node0", KeyType: "ssh-ed25519", Key: "AAAAC3NzaC1lZDI1NTE5AAAAoriginal",
	}); err != nil {
		t.Fatalf("re-record same key: %v", err)
	}

	// A CHANGED key for an existing (host, type) is refused, never silently
	// overwritten — a changed key is a rekey the operator confirms, or a MITM.
	if err := s.RecordKnownHost(ctx, store.KnownHost{
		Host: "node0", KeyType: "ssh-ed25519", Key: "AAAAC3NzaC1lZDI1NTE5AAAArekeyed",
	}); !errors.Is(err, store.ErrKnownHostChanged) {
		t.Fatalf("changed key: want ErrKnownHostChanged, got %v", err)
	}

	// RotateKnownHost replaces the value after out-of-band confirmation, still
	// one row per (host, type).
	if err := s.RotateKnownHost(ctx, store.KnownHost{
		Host: "node0", KeyType: "ssh-ed25519", Key: "AAAAC3NzaC1lZDI1NTE5AAAArekeyed",
	}); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	hosts, err := s.ListKnownHosts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("kept %d rows, want 2 (rekey replaced, not appended)", len(hosts))
	}

	rendered := store.RenderKnownHosts(hosts)
	if !strings.Contains(rendered, "node0 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAArekeyed") {
		t.Errorf("render missing the rekeyed ed25519 line:\n%s", rendered)
	}
	if strings.Contains(rendered, "original") {
		t.Errorf("render still carries the replaced key:\n%s", rendered)
	}
	if !strings.Contains(rendered, "node0 ssh-rsa AAAAB3NzaC1yc2Ersa") {
		t.Errorf("render missing the rsa line:\n%s", rendered)
	}

	if err := s.DeleteKnownHost(ctx, "node0"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, err := s.ListKnownHosts(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("delete left %d rows, want 0", len(after))
	}
}
