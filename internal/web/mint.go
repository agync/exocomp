package web

import (
	"context"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// MintToken creates a token row and returns the one-time string to hand the
// operator. The secret is generated, argon2id-hashed, and stored; the returned
// string is the only time the plaintext exists outside the operator's paste
// buffer. A zero expires means the token never expires.
func MintToken(ctx context.Context, st *store.Store, name, scope, host string, expires time.Duration) (string, store.APIToken, error) {
	secret, err := newSecret()
	if err != nil {
		return "", store.APIToken{}, err
	}
	hash, err := hashSecret(secret)
	if err != nil {
		return "", store.APIToken{}, err
	}

	row := store.APIToken{Name: name, Scope: scope, Host: host, Prefix: prefixOf(secret), Hash: hash}
	if expires > 0 {
		exp := time.Now().UTC().Add(expires)
		row.ExpiresAt = &exp
	}

	created, err := st.CreateToken(ctx, row)
	if err != nil {
		return "", store.APIToken{}, err
	}
	return formatToken(created.ID, secret), created, nil
}

// RotateToken reseals a fresh secret onto the existing token named name, keeping
// its id, name, and scope. The old secret stops authenticating and sessions
// bootstrapped from it are dropped; a revoked token is brought back live. The
// returned string is the one-time plaintext for the new secret. setExpiry
// replaces the token's expiry (a zero duration clears it); when false the
// existing expiry is kept. A missing name returns store.ErrNotFound.
func RotateToken(ctx context.Context, st *store.Store, name string, expires time.Duration, setExpiry bool) (string, store.APIToken, error) {
	secret, err := newSecret()
	if err != nil {
		return "", store.APIToken{}, err
	}
	hash, err := hashSecret(secret)
	if err != nil {
		return "", store.APIToken{}, err
	}

	var expiresAt *time.Time
	if setExpiry && expires > 0 {
		exp := time.Now().UTC().Add(expires)
		expiresAt = &exp
	}

	row, err := st.RotateToken(ctx, name, hash, prefixOf(secret), expiresAt, setExpiry, time.Now().UTC())
	if err != nil {
		return "", store.APIToken{}, err
	}
	return formatToken(row.ID, secret), row, nil
}
