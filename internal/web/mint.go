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
func MintToken(ctx context.Context, st *store.Store, name, scope string, expires time.Duration) (string, store.APIToken, error) {
	secret, err := newSecret()
	if err != nil {
		return "", store.APIToken{}, err
	}
	hash, err := hashSecret(secret)
	if err != nil {
		return "", store.APIToken{}, err
	}

	row := store.APIToken{Name: name, Scope: scope, Prefix: prefixOf(secret), Hash: hash}
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
