package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestHostQuarantineUntilRepin is the 8d store gate: a runtime host-key mismatch
// quarantines the host, a run against it is blocked while quarantined, the
// recorded (old) and presented (new) fingerprints both surface, and only an
// approved re-pin (RotateKnownHost) lifts the quarantine.
func TestHostQuarantineUntilRepin(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	// A host pinned on first sight. Its recorded fingerprint is what the operator
	// compares a later presented key against.
	recorded := store.KnownHost{Host: "web1", KeyType: "ssh-ed25519", Key: "AAAAC3NzaC1lZDI1NTE5AAAAoriginal"}
	if err := s.RecordKnownHost(ctx, recorded); err != nil {
		t.Fatalf("record: %v", err)
	}
	oldFP := recorded.Fingerprint()
	if oldFP == "" || !strings.HasPrefix(oldFP, "SHA256:") {
		t.Fatalf("recorded fingerprint malformed: %q", oldFP)
	}

	// The queue's admission check sees no quarantine yet.
	if q, err := s.IsHostQuarantined(ctx, "web1"); err != nil || q {
		t.Fatalf("host quarantined before any mismatch: q=%v err=%v", q, err)
	}

	// A run trips the mismatch: the host presented a different key. Quarantine it
	// with both fingerprints.
	const newFP = "SHA256:presented9d3f1anewfingerprintvalueAAAA"
	if err := s.QuarantineHost(ctx, store.HostQuarantine{
		Host:           "web1",
		Reason:         "host key changed at runtime",
		OldFingerprint: oldFP,
		NewFingerprint: newFP,
	}); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	// The host is now blocked, and both fingerprints surface — old vs new.
	if q, err := s.IsHostQuarantined(ctx, "web1"); err != nil || !q {
		t.Fatalf("host not quarantined after mismatch: q=%v err=%v", q, err)
	}
	got, err := s.GetHostQuarantine(ctx, "web1")
	if err != nil {
		t.Fatalf("get quarantine: %v", err)
	}
	if got.OldFingerprint != oldFP || got.NewFingerprint != newFP {
		t.Fatalf("fingerprints did not surface: old=%q new=%q", got.OldFingerprint, got.NewFingerprint)
	}
	if got.OldFingerprint == got.NewFingerprint {
		t.Fatal("old and new fingerprints must differ — that is the mismatch")
	}

	// A re-record short of a rotate does NOT lift the quarantine: the recorded
	// key is unchanged and the block stands.
	if err := s.RecordKnownHost(ctx, store.KnownHost{
		Host: "web1", KeyType: "ssh-ed25519", Key: "AAAAC3NzaC1lZDI1NTE5AAAAattacker",
	}); !errors.Is(err, store.ErrKnownHostChanged) {
		t.Fatalf("record of a different key: want ErrKnownHostChanged, got %v", err)
	}
	if q, err := s.IsHostQuarantined(ctx, "web1"); err != nil || !q {
		t.Fatal("quarantine lifted without an approved re-pin")
	}

	// The approved re-pin (RotateKnownHost, reached only through the gate) both
	// replaces the key and lifts the quarantine, in one step.
	if err := s.RotateKnownHost(ctx, store.KnownHost{
		Host: "web1", KeyType: "ssh-ed25519", Key: "AAAAC3NzaC1lZDI1NTE5AAAArekeyed",
	}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if q, err := s.IsHostQuarantined(ctx, "web1"); err != nil || q {
		t.Fatalf("approved re-pin did not lift the quarantine: q=%v err=%v", q, err)
	}
	if _, err := s.GetHostQuarantine(ctx, "web1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("quarantine row survived the re-pin: %v", err)
	}
}

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
