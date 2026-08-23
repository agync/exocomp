package reactive_test

import (
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/reactive"
)

// driftPages returns the notifications the drift transport identity carries,
// i.e. the opening pages and the closes that reuse the same Source/Host so a
// GoAlert incident lines its two halves up.
func driftPages(sent []notify.Notification) []notify.Notification {
	var out []notify.Notification
	for _, n := range sent {
		if n.Source == reactive.SourceDrift {
			out = append(out, n)
		}
	}
	return out
}

// TestDriftPagesStableIdentityThenAutoCloses is the whole story: a drifted
// workspace pages once with a stable per-workspace identity that opens, a
// later clean plan for that workspace closes that exact incident, and a second
// sweep closes nothing — the event table is the checkpoint.
func TestDriftPagesStableIdentityThenAutoCloses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})

	// A plan that found changes. No rule is armed, so the only notification is
	// the drift page itself, not an approval the gating would raise.
	h.planJob(t, "prod", true)
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (open): %v", err)
	}

	opens := driftPages(h.sent.sent)
	if len(opens) != 1 {
		t.Fatalf("drift raised %d pages, want 1 opening page", len(opens))
	}
	open := opens[0]
	if open.Resolved {
		t.Fatal("opening drift page is Resolved; it must open the incident, not close it")
	}
	if open.Source != reactive.SourceDrift || open.Host != "prod" {
		t.Fatalf("opening identity = %q/%q, want %q/prod", open.Source, open.Host, reactive.SourceDrift)
	}

	// The workspace is reconciled: a later plan finds nothing.
	h.planJob(t, "prod", false)
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (close): %v", err)
	}

	pages := driftPages(h.sent.sent)
	if len(pages) != 2 {
		t.Fatalf("after clean plan there are %d drift pages, want 2 (one open, one close)", len(pages))
	}
	closing := pages[1]
	if !closing.Resolved {
		t.Fatalf("close page is not Resolved: %q", closing.Title)
	}
	if closing.Source != open.Source || closing.Host != open.Host {
		t.Fatalf("close identity = %q/%q, want the opening page's %q/%q",
			closing.Source, closing.Host, open.Source, open.Host)
	}

	// A second sweep over the same clean plan closes nothing: the cleared
	// checkpoint already resolves that incident.
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (idempotent): %v", err)
	}
	if again := driftPages(h.sent.sent); len(again) != 2 {
		t.Fatalf("second sweep produced %d drift pages, want the same 2", len(again))
	}
}

// TestCleanPlanWithoutPriorDriftClosesNothing pins that a workspace that never
// drifted raises no close: there is no open incident for a clean plan to clear.
func TestCleanPlanWithoutPriorDriftClosesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.planJob(t, "prod", false)
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift: %v", err)
	}
	if pages := driftPages(h.sent.sent); len(pages) != 0 {
		t.Fatalf("a clean plan with no prior drift produced %d drift pages, want 0", len(pages))
	}
}

// TestStillDriftingRaisesNoClose pins that a workspace whose latest plan still
// finds changes stays open: only a clean plan clears it, and there has not been
// one. Every drift page is an open, never a close.
func TestStillDriftingRaisesNoClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.planJob(t, "prod", true)
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	h.planJob(t, "prod", true)
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}

	pages := driftPages(h.sent.sent)
	if len(pages) == 0 {
		t.Fatal("a drifted workspace raised no drift page")
	}
	for i, n := range pages {
		if n.Resolved {
			t.Fatalf("drift page %d is a close, but the workspace never went clean", i)
		}
	}
}
