package wire

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// openTestStore opens a throwaway store for a wire projection test.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestJobViewExpiry is the GATE for JobView.expires_at (mobile-realign ask 1):
// an awaiting_approval job's projection carries the pending approval's
// fail-closed deadline; a terminal job omits it; a zero time leaves the field
// nil (an approval that races away emits the honest empty form).
func TestJobViewExpiry(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	gated, err := st.CreateJob(ctx, store.NewJob{Runbook: "gated", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := st.TransitionJob(ctx, gated.ID, store.StateQueued, store.StateAwaitingApproval); err != nil {
		t.Fatalf("TransitionJob: %v", err)
	}
	expires := time.Now().Add(30 * time.Minute).UTC()
	if _, err := st.CreateApproval(ctx, store.Approval{
		JobID: gated.ID, ExpiresAt: expires, TokenHash: "test-token",
	}); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	gated, err = st.GetJob(ctx, gated.ID)
	if err != nil {
		t.Fatalf("GetJob gated: %v", err)
	}

	// Awaiting approval carries the expiry; a zero-time expiry leaves it nil.
	v := NewJobViewResolved(ctx, st, gated)
	if v.ExpiresAt == nil {
		t.Fatal("awaiting_approval job has no expires_at")
	}
	parsed, err := time.Parse(time.RFC3339, *v.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", *v.ExpiresAt, err)
	}
	// The wire field renders at second precision (time.RFC3339 drops fractions),
	// so compare seconds — the sub-second is not part of the contract.
	if parsed.Unix() != expires.Unix() {
		t.Fatalf("expires_at = %q, want %q", *v.ExpiresAt, expires.Format(time.RFC3339))
	}

	// Terminal jobs omit the field entirely.
	done, err := st.CreateJob(ctx, store.NewJob{Runbook: "done", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("CreateJob done: %v", err)
	}
	if err := st.TransitionJob(ctx, done.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("TransitionJob done: %v", err)
	}
	if err := st.TransitionJob(ctx, done.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("TransitionJob done: %v", err)
	}
	done, err = st.GetJob(ctx, done.ID)
	if err != nil {
		t.Fatalf("GetJob done: %v", err)
	}
	if v := NewJobViewResolved(ctx, st, done); v.ExpiresAt != nil {
		t.Fatal("a terminal job must not carry expires_at")
	}

	// WithExpiry with a zero time leaves nil (raced-away approval).
	var z JobView
	z.WithExpiry(time.Time{})
	if z.ExpiresAt != nil {
		t.Fatal("WithExpiry(zero) must leave expires_at nil")
	}
}

// TestJobViewsResolvedBatches asserts the list projection resolves every
// awaiting_approval row with ONE approval read and leaves terminal rows clean.
func TestJobViewsResolvedBatches(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	gated, err := st.CreateJob(ctx, store.NewJob{Runbook: "gated", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := st.TransitionJob(ctx, gated.ID, store.StateQueued, store.StateAwaitingApproval); err != nil {
		t.Fatalf("TransitionJob: %v", err)
	}
	expires := time.Now().Add(15 * time.Minute).UTC()
	if _, err := st.CreateApproval(ctx, store.Approval{
		JobID: gated.ID, ExpiresAt: expires, TokenHash: "test-token",
	}); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	done, err := st.CreateJob(ctx, store.NewJob{Runbook: "done", Kind: "shell", Trigger: "manual"})
	if err != nil {
		t.Fatalf("CreateJob done: %v", err)
	}
	if err := st.TransitionJob(ctx, done.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("TransitionJob done: %v", err)
	}
	if err := st.TransitionJob(ctx, done.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("TransitionJob done: %v", err)
	}

	rows, err := st.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	views := NewJobViewsResolved(ctx, st, rows)
	var gotExpiry, gotClean bool
	for i, v := range views {
		if v.ID == gated.ID {
			if v.ExpiresAt == nil {
				t.Fatal("list projection missed the awaiting_approval expiry")
			}
			gotExpiry = true
		}
		if v.ID == done.ID {
			if v.ExpiresAt != nil {
				t.Fatal("list projection leaked an expiry onto a terminal job")
			}
			gotClean = true
		}
		if views[i].State == string(store.StateSucceeded) && v.ExpiresAt != nil {
			t.Fatal("a succeeded job must never carry expires_at")
		}
	}
	if !gotExpiry || !gotClean {
		t.Fatal("list projection did not cover both the gated and the terminal job")
	}
}
