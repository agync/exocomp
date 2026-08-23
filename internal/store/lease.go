package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Lease errors, matched by callers with errors.Is.
var (
	// ErrLeaseHeld is returned by AcquireLease when a LIVE lease is held by a
	// different holder and force was not set. The returned Lease is that live
	// one, so the caller can name the holder and when it was last renewed.
	ErrLeaseHeld = errors.New("store: serve lease held by another holder")
	// ErrLeaseLost is returned by RenewLease when the row is no longer this
	// holder's — another daemon force-took it. The renewing daemon must stand
	// down: it is no longer the active one, and must not write over the taker.
	ErrLeaseLost = errors.New("store: serve lease lost to another holder")
)

// leaseRowID pins the single-active lease to one row (migration 0023).
const leaseRowID = 1

// Lease is the cross-host single-active lease. The active daemon holds it and
// renews RenewedAt within TTL; a starting daemon that finds a live one refuses.
type Lease struct {
	HolderID   string
	AcquiredAt time.Time
	RenewedAt  time.Time
	TTL        time.Duration
}

// Live reports whether the lease was still within its TTL at now. A lease that
// is not Live is STALE and may be taken by any daemon.
func (l Lease) Live(now time.Time) bool {
	return now.Before(l.Expiry())
}

// Expiry is when the lease goes stale unless it is renewed first.
func (l Lease) Expiry() time.Time {
	return l.RenewedAt.Add(l.TTL)
}

// GetLease returns the current lease, or ErrNotFound when none is held.
func (s *Store) GetLease(ctx context.Context) (Lease, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT holder_id, acquired_at, renewed_at, ttl_ms FROM serve_lease WHERE id = ?`, leaseRowID)
	l, err := scanLease(row)
	if err != nil {
		return Lease{}, wrap("get lease", err)
	}
	return l, nil
}

// AcquireLease takes the single-active lease for holderID with the given ttl,
// judged against now. It takes the lease when it is free, already this holder's,
// or STALE (now past its expiry). It refuses with ErrLeaseHeld — returning the
// live lease so the caller can name the holder — when a different holder's lease
// is still LIVE, unless force is set. force takes it regardless: the operator's
// explicit override for a holder known to be gone. acquired_at is preserved
// while the same holder renews and reset to now on every takeover, so it always
// reads as when the current holder became active.
//
// The whole read-decide-write is one immediate transaction, so two daemons
// racing to acquire the same free-or-stale lease cannot both win it.
func (s *Store) AcquireLease(ctx context.Context, holderID string, ttl time.Duration, now time.Time, force bool) (Lease, error) {
	if holderID == "" {
		return Lease{}, errors.New("store: acquire lease: empty holder id")
	}
	if ttl <= 0 {
		return Lease{}, fmt.Errorf("store: acquire lease: ttl %s must be positive", ttl)
	}
	now = now.UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, wrap("acquire lease", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		`SELECT holder_id, acquired_at, renewed_at, ttl_ms FROM serve_lease WHERE id = ?`, leaseRowID)
	current, err := scanLease(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Free: nobody holds it.
	case err != nil:
		return Lease{}, wrap("acquire lease", err)
	case current.HolderID != holderID && current.Live(now) && !force:
		return current, fmt.Errorf("store: acquire lease: %w", ErrLeaseHeld)
	}

	acquiredAt := now
	if err == nil && current.HolderID == holderID {
		// The same daemon re-acquiring keeps the moment it first went active.
		acquiredAt = current.AcquiredAt
	}

	taken := Lease{HolderID: holderID, AcquiredAt: acquiredAt, RenewedAt: now, TTL: ttl}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO serve_lease (id, holder_id, acquired_at, renewed_at, ttl_ms)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			holder_id = excluded.holder_id,
			acquired_at = excluded.acquired_at,
			renewed_at = excluded.renewed_at,
			ttl_ms = excluded.ttl_ms`,
		leaseRowID, taken.HolderID, taken.AcquiredAt.UnixMilli(), taken.RenewedAt.UnixMilli(), ttl.Milliseconds()); err != nil {
		return Lease{}, wrap("acquire lease", err)
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, wrap("acquire lease", err)
	}
	return taken, nil
}

// RenewLease refreshes renewed_at to now, but only while holderID still holds
// the lease. A row held by someone else (a force-takeover) or gone leaves zero
// rows updated and returns ErrLeaseLost, so the caller stands down instead of
// writing over the new holder.
func (s *Store) RenewLease(ctx context.Context, holderID string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE serve_lease SET renewed_at = ? WHERE id = ? AND holder_id = ?`,
		now.UTC().UnixMilli(), leaseRowID, holderID)
	if err != nil {
		return wrap("renew lease", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("renew lease", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: renew lease: %w", ErrLeaseLost)
	}
	return nil
}

// ReleaseLease drops the lease if holderID holds it, so a cleanly shut-down
// daemon frees the fence at once and a standby need not wait out the TTL. A row
// held by someone else is left untouched — release is idempotent and never
// steals from a holder that took over.
func (s *Store) ReleaseLease(ctx context.Context, holderID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM serve_lease WHERE id = ? AND holder_id = ?`, leaseRowID, holderID)
	if err != nil {
		return wrap("release lease", err)
	}
	return nil
}

func scanLease(row rowScanner) (Lease, error) {
	var (
		l                     Lease
		acquiredAt, renewedAt int64
		ttlMs                 int64
	)
	if err := row.Scan(&l.HolderID, &acquiredAt, &renewedAt, &ttlMs); err != nil {
		return Lease{}, err
	}
	l.AcquiredAt = time.UnixMilli(acquiredAt).UTC()
	l.RenewedAt = time.UnixMilli(renewedAt).UTC()
	l.TTL = time.Duration(ttlMs) * time.Millisecond
	return l, nil
}
