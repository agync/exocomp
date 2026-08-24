package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestMigration0024UpgradesFrom22 pins a database at user_version 22, then opens
// it with the current binary: the migration must apply and the new lease must be
// usable through the store, which is what an operator upgrading in place gets.
func TestMigration0024UpgradesFrom22(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigrations(t, dbPath, 22)

	raw, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var count int
	err = raw.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'serve_lease'`).Scan(&count)
	if err != nil {
		t.Fatalf("probe pre-migration schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	if count != 0 {
		t.Fatalf("serve_lease table exists at user_version 22, want it introduced by 0024")
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open at v22 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 24 {
		t.Fatalf("schema version = %d after open, want >= 24", version)
	}

	if _, err := s.GetLease(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fresh lease table: %v, want ErrNotFound", err)
	}
	now := time.Now().UTC()
	if _, err := s.AcquireLease(ctx, "host-a/pid-1", 30*time.Second, now, false); err != nil {
		t.Fatalf("acquire lease on migrated schema: %v", err)
	}
	got, err := s.GetLease(ctx)
	if err != nil || got.HolderID != "host-a/pid-1" {
		t.Fatalf("get lease = %+v, %v, want the one just acquired", got, err)
	}
}

// TestLeaseFreeAcquireAndRefuseLive is the fence's core: a free lease is taken,
// and while it is live another holder is refused with the live lease named.
func TestLeaseFreeAcquireAndRefuseLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	t0 := time.Unix(1_700_000_000, 0).UTC()
	first, err := s.AcquireLease(ctx, "host-a/pid-1", 30*time.Second, t0, false)
	if err != nil {
		t.Fatalf("acquire free lease: %v", err)
	}
	if first.HolderID != "host-a/pid-1" || !first.AcquiredAt.Equal(t0) || !first.RenewedAt.Equal(t0) {
		t.Fatalf("acquired lease = %+v, want holder host-a/pid-1 at t0", first)
	}

	// A second host, while the first is live, is refused and told who holds it.
	held, err := s.AcquireLease(ctx, "host-b/pid-9", 30*time.Second, t0.Add(5*time.Second), false)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second acquire while live: %v, want ErrLeaseHeld", err)
	}
	if held.HolderID != "host-a/pid-1" {
		t.Errorf("refusal named holder %q, want host-a/pid-1", held.HolderID)
	}
	if !held.RenewedAt.Equal(t0) {
		t.Errorf("refusal reports last renew %s, want t0 %s", held.RenewedAt, t0)
	}

	// The live holder still owns it: nothing was taken.
	cur, err := s.GetLease(ctx)
	if err != nil || cur.HolderID != "host-a/pid-1" {
		t.Fatalf("lease after refusal = %+v, %v, want still host-a/pid-1", cur, err)
	}
}

// TestLeaseStaleIsTaken proves a lease past its TTL is free for the taking, and
// that the taker's acquired_at resets to the takeover moment.
func TestLeaseStaleIsTaken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	t0 := time.Unix(1_700_000_000, 0).UTC()
	if _, err := s.AcquireLease(ctx, "host-a/pid-1", 30*time.Second, t0, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// One second before expiry it is still live and refuses.
	if _, err := s.AcquireLease(ctx, "host-b/pid-9", 30*time.Second, t0.Add(29*time.Second), false); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("acquire just before expiry: %v, want ErrLeaseHeld", err)
	}

	// Past expiry plus the skew guard it is stale: the next host takes it
	// without force. (Exactly at expiry is still within the skew margin — see
	// TestLeaseSkewGuardKeepsLiveWithinMargin.)
	takeover := t0.Add(30*time.Second + LeaseSkewMargin + time.Second)
	taken, err := s.AcquireLease(ctx, "host-b/pid-9", 30*time.Second, takeover, false)
	if err != nil {
		t.Fatalf("acquire stale lease: %v", err)
	}
	if taken.HolderID != "host-b/pid-9" || !taken.AcquiredAt.Equal(takeover) {
		t.Fatalf("stale takeover = %+v, want host-b/pid-9 acquired at takeover", taken)
	}
}

// TestLeaseForceTakesLive proves a live lease held by another holder is taken
// only when force is set — the explicit operator override.
func TestLeaseForceTakesLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	t0 := time.Unix(1_700_000_000, 0).UTC()
	if _, err := s.AcquireLease(ctx, "host-a/pid-1", 30*time.Second, t0, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Without force, refused while live.
	if _, err := s.AcquireLease(ctx, "host-b/pid-9", 30*time.Second, t0.Add(time.Second), false); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("force=false while live: %v, want ErrLeaseHeld", err)
	}
	// With force, taken over.
	taken, err := s.AcquireLease(ctx, "host-b/pid-9", 30*time.Second, t0.Add(time.Second), true)
	if err != nil {
		t.Fatalf("force acquire: %v", err)
	}
	if taken.HolderID != "host-b/pid-9" {
		t.Fatalf("force takeover holder = %q, want host-b/pid-9", taken.HolderID)
	}
}

// TestLeaseReacquireKeepsAcquiredAt proves the active daemon re-acquiring its own
// lease keeps the moment it first went active while moving renewed_at forward.
func TestLeaseReacquireKeepsAcquiredAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	t0 := time.Unix(1_700_000_000, 0).UTC()
	if _, err := s.AcquireLease(ctx, "host-a/pid-1", 30*time.Second, t0, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	again, err := s.AcquireLease(ctx, "host-a/pid-1", 30*time.Second, t0.Add(10*time.Second), false)
	if err != nil {
		t.Fatalf("re-acquire own lease: %v", err)
	}
	if !again.AcquiredAt.Equal(t0) {
		t.Errorf("acquired_at = %s after re-acquire, want unchanged t0 %s", again.AcquiredAt, t0)
	}
	if !again.RenewedAt.Equal(t0.Add(10 * time.Second)) {
		t.Errorf("renewed_at = %s after re-acquire, want t0+10s", again.RenewedAt)
	}
}

// TestRenewGuardsHolder proves renew keeps a live lease alive for its holder but
// refuses (ErrLeaseLost) once another holder has taken over — the signal a
// force-taken daemon needs to stand down.
func TestRenewGuardsHolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	t0 := time.Unix(1_700_000_000, 0).UTC()
	if _, err := s.AcquireLease(ctx, "host-a/pid-1", 30*time.Second, t0, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.RenewLease(ctx, "host-a/pid-1", t0.Add(5*time.Second)); err != nil {
		t.Fatalf("renew own live lease: %v", err)
	}

	// Another host force-takes it; the original holder's renew now loses.
	if _, err := s.AcquireLease(ctx, "host-b/pid-9", 30*time.Second, t0.Add(6*time.Second), true); err != nil {
		t.Fatalf("force takeover: %v", err)
	}
	if err := s.RenewLease(ctx, "host-a/pid-1", t0.Add(7*time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renew after being taken over: %v, want ErrLeaseLost", err)
	}
	// The taker's own renew still works.
	if err := s.RenewLease(ctx, "host-b/pid-9", t0.Add(8*time.Second)); err != nil {
		t.Fatalf("taker renew: %v", err)
	}
}

// TestReleaseFreesTheFence proves a clean release frees the lease at once, so a
// standby acquires it without waiting out the TTL, and that release never steals
// from a holder that took over.
func TestReleaseFreesTheFence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	t0 := time.Unix(1_700_000_000, 0).UTC()
	if _, err := s.AcquireLease(ctx, "host-a/pid-1", 30*time.Second, t0, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.ReleaseLease(ctx, "host-a/pid-1"); err != nil {
		t.Fatalf("release own lease: %v", err)
	}

	// Freed: the standby takes it immediately, still well within the TTL window.
	if _, err := s.AcquireLease(ctx, "host-b/pid-9", 30*time.Second, t0.Add(time.Second), false); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}

	// A stale release from the old holder must not evict the new one.
	if err := s.ReleaseLease(ctx, "host-a/pid-1"); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	cur, err := s.GetLease(ctx)
	if err != nil || cur.HolderID != "host-b/pid-9" {
		t.Fatalf("lease after stale release = %+v, %v, want still host-b/pid-9", cur, err)
	}
}

// TestLeaseSkewGuardKeepsLiveWithinMargin proves a reader whose clock has
// drifted ahead of the holder's stays within LeaseSkewMargin of the lease's
// nominal expiry and still judges it live, so a same-margin-fast standby does
// not take over a primary that is still renewing.
func TestLeaseSkewGuardKeepsLiveWithinMargin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	t0 := time.Unix(1_700_000_000, 0).UTC()
	if _, err := s.AcquireLease(ctx, "host-a/pid-1", 30*time.Second, t0, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// TTL expires at t0+30s; a reader within the skew margin past that must
	// still refuse, as if the lease were live.
	past := t0.Add(30*time.Second + LeaseSkewMargin - time.Second)
	if _, err := s.AcquireLease(ctx, "host-b/pid-9", 30*time.Second, past, false); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("acquire %s past expiry (within skew margin): %v, want ErrLeaseHeld", LeaseSkewMargin-time.Second, err)
	}
}

// TestLeaseSkewGuardStaleBeyondMargin proves a reader past TTL+LeaseSkewMargin
// still judges the lease stale and takes over: the guard bounds the risk of a
// fast reader clock without ever hiding a genuinely dead holder forever.
func TestLeaseSkewGuardStaleBeyondMargin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	t0 := time.Unix(1_700_000_000, 0).UTC()
	if _, err := s.AcquireLease(ctx, "host-a/pid-1", 30*time.Second, t0, false); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	past := t0.Add(30*time.Second + LeaseSkewMargin + time.Second)
	taken, err := s.AcquireLease(ctx, "host-b/pid-9", 30*time.Second, past, false)
	if err != nil {
		t.Fatalf("acquire past ttl+skew margin: %v", err)
	}
	if taken.HolderID != "host-b/pid-9" {
		t.Fatalf("takeover holder = %q, want host-b/pid-9", taken.HolderID)
	}
}

// TestLeaseRejectsBadInput proves the store guards the lease against a missing
// holder id and a non-positive TTL.
func TestLeaseRejectsBadInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now().UTC()
	if _, err := s.AcquireLease(ctx, "", 30*time.Second, now, false); err == nil {
		t.Error("acquired a lease with an empty holder id")
	}
	if _, err := s.AcquireLease(ctx, "host-a/pid-1", 0, now, false); err == nil {
		t.Error("acquired a lease with a zero TTL")
	}
}
