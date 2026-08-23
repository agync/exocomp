package reactive_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// seedSecret writes a sealed-secret row with the given expiry. The sweep reads
// only the name, kind, and deadline, so the sealed value is a placeholder.
func (h *harness) seedSecret(t *testing.T, name string, expiresAt *time.Time) store.SecretRow {
	t.Helper()
	sec, err := h.store.CreateSecret(context.Background(), store.SecretRow{
		Name:       name,
		Kind:       "token",
		Nonce:      []byte("nonce-placeholder"),
		Ciphertext: []byte("ct-placeholder"),
		AAD:        name + "|token",
		ExpiresAt:  expiresAt,
	})
	if err != nil {
		t.Fatalf("seed secret %q: %v", name, err)
	}
	return sec
}

// TestExpiredSecretPagesExactlyOnce is the loop the sweep exists for: an expired
// secret pages the operator once and never again, over the same event-table
// checkpoint SweepJobOutcomes uses.
func TestExpiredSecretPagesExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	past := h.nowValue.Add(-1 * time.Hour)
	h.seedSecret(t, "prod/api", &past)

	raised, err := h.engine.SweepSecretExpiry(ctx)
	if err != nil {
		t.Fatalf("sweep secret expiry: %v", err)
	}
	if raised != 1 {
		t.Fatalf("raised %d pages, want 1", raised)
	}
	if len(h.sent.sent) != 1 {
		t.Fatalf("transport saw %d notifications, want 1", len(h.sent.sent))
	}

	note := h.sent.sent[0]
	if !strings.Contains(note.Title, "secret expired") || !strings.Contains(note.Title, "prod/api") {
		t.Fatalf("title = %q, want it to name the expired secret", note.Title)
	}
	if !strings.Contains(note.Body, "token") {
		t.Fatalf("body = %q, want the kind in it", note.Body)
	}
	if note.Priority != notify.PriorityHigh {
		t.Fatalf("priority = %d, want high for an expired secret", note.Priority)
	}
	// Attributed by secret name so a scoped mute holds it.
	if note.Source != reactive.SourceSecretExpiry {
		t.Fatalf("source = %q, want %q", note.Source, reactive.SourceSecretExpiry)
	}
	if note.Host != "prod/api" {
		t.Fatalf("host = %q, want the secret name", note.Host)
	}
	// An opening page, not a resolve: it must not close a GoAlert incident.
	if note.Resolved {
		t.Fatal("expiry page is Resolved; it should open, not close")
	}

	// Idempotent across repeated sweeps: the event table is the checkpoint.
	again, err := h.engine.SweepSecretExpiry(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("second sweep raised %d pages, want 0", again)
	}
	if len(h.sent.sent) != 1 {
		t.Fatalf("transport saw %d notifications after the second sweep, want 1", len(h.sent.sent))
	}
}

// TestExpiringSecretPagesAsLeadWarning pins the lead warning: a secret inside the
// window pages once, at a lower priority than an outright expiry.
func TestExpiringSecretPagesAsLeadWarning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	soon := h.nowValue.Add(3 * 24 * time.Hour)
	h.seedSecret(t, "prod/api", &soon)

	raised, err := h.engine.SweepSecretExpiry(ctx)
	if err != nil {
		t.Fatalf("sweep secret expiry: %v", err)
	}
	if raised != 1 {
		t.Fatalf("raised %d pages, want 1", raised)
	}
	note := h.sent.sent[0]
	if !strings.Contains(note.Title, "secret expiring") {
		t.Fatalf("title = %q, want a lead warning", note.Title)
	}
	if note.Priority >= notify.PriorityHigh {
		t.Fatalf("priority = %d, want it below the expired page's high", note.Priority)
	}
}

// TestFarSecretAndNeverExpiringPageNothing is the other half of the contract: a
// deadline past the window, and a secret that never expires, are not alerts.
func TestFarSecretAndNeverExpiringPageNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	far := h.nowValue.Add(30 * 24 * time.Hour)
	h.seedSecret(t, "prod/far", &far)
	h.seedSecret(t, "prod/forever", nil)

	raised, err := h.engine.SweepSecretExpiry(ctx)
	if err != nil {
		t.Fatalf("sweep secret expiry: %v", err)
	}
	if raised != 0 {
		t.Fatalf("raised %d pages for a far/never-expiring secret, want 0", raised)
	}
	if len(h.sent.sent) != 0 {
		t.Fatalf("transport saw %d notifications, want 0", len(h.sent.sent))
	}
}

// TestExpiringThenExpiredPagesBoth pins that the lead warning and the expiry are
// distinct conditions: a secret paged on while expiring still pages when it later
// expires, because the higher-priority page must not be swallowed by the earlier
// one sharing the secret name.
func TestExpiringThenExpiredPagesBoth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	deadline := h.nowValue.Add(2 * 24 * time.Hour)
	h.seedSecret(t, "prod/api", &deadline)

	if _, err := h.engine.SweepSecretExpiry(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if len(h.sent.sent) != 1 || !strings.Contains(h.sent.sent[0].Title, "expiring") {
		t.Fatalf("first sweep should page one lead warning, got %d", len(h.sent.sent))
	}

	// The deadline passes.
	h.advance(3 * 24 * time.Hour)
	if _, err := h.engine.SweepSecretExpiry(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(h.sent.sent) != 2 {
		t.Fatalf("after expiry, transport saw %d notifications, want 2", len(h.sent.sent))
	}
	if !strings.Contains(h.sent.sent[1].Title, "expired") {
		t.Fatalf("second page title = %q, want the expiry", h.sent.sent[1].Title)
	}
}
