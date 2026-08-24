package reactive

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

type stubDeriver struct{}

func (stubDeriver) Derive(purpose, id string) []byte {
	sum := sha256.Sum256([]byte(purpose + ":" + id))
	return sum[:]
}

type discardTransport struct{}

func (discardTransport) Name() string { return "discard" }
func (discardTransport) Publish(context.Context, notify.Notification) error {
	return nil
}

// republished is unexported process state (engine.go), so proving it stays
// bounded needs a white-box test: SweepApprovals's return counts alone can't
// distinguish "pruned" from "leaked but no longer touched" once an approval
// leaves the pending set.
func TestSweepApprovalsPrunesDecidedFromRepublished(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      1,
		PollInterval: time.Hour,
		Logger:       slog.New(slog.DiscardHandler),
	})

	now := time.Now().UTC()
	e, err := New(Options{
		Store:       st,
		Queue:       q,
		Deriver:     stubDeriver{},
		Notifier:    notify.New(slog.New(slog.DiscardHandler), discardTransport{}),
		ApprovalTTL: 10 * time.Minute,
		Clock:       func() time.Time { return now },
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	var ids []int64
	for i := 0; i < 3; i++ {
		job, err := st.CreateJob(ctx, store.NewJob{Runbook: "x", Kind: "shell", Trigger: "test"})
		if err != nil {
			t.Fatalf("create job: %v", err)
		}
		a, err := st.CreateApproval(ctx, store.Approval{
			JobID:       job.ID,
			RequestedAt: now,
			ExpiresAt:   now.Add(10 * time.Minute),
			TokenHash:   "deadbeef",
		})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		ids = append(ids, a.ID)
	}

	// Past half the TTL: every pending approval gets its one reminder.
	now = now.Add(6 * time.Minute)
	if _, republished, err := e.SweepApprovals(ctx); err != nil || republished != 3 {
		t.Fatalf("sweep: republished=%d err=%v, want 3", republished, err)
	}
	if len(e.republished) != 3 {
		t.Fatalf("republished map = %d entries, want 3", len(e.republished))
	}

	// Two of the three settle — a terminal approval no longer belongs in the
	// pending set the next sweep reads.
	for _, id := range ids[:2] {
		if _, err := st.DecideApproval(ctx, id, store.ApprovalApproved, "test", ""); err != nil {
			t.Fatalf("decide approval %d: %v", id, err)
		}
	}

	if _, republished, err := e.SweepApprovals(ctx); err != nil || republished != 0 {
		t.Fatalf("second sweep: republished=%d err=%v, want 0 (no re-reminder)", republished, err)
	}
	if len(e.republished) != 1 {
		t.Fatalf("republished map after decide+sweep = %d entries, want 1", len(e.republished))
	}
	if !e.republished[ids[2]] {
		t.Fatal("the still-pending approval's own entry was evicted")
	}
}
