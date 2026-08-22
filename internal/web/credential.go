package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// tokenScheme prefixes every minted token so a leaked one is greppable and a
// pasted line that is not a token is rejected before any crypto runs.
const tokenScheme = "exo"

// secretBytes is the entropy behind a token: 24 bytes, 192 bits, well past
// guessing. The row id travels alongside it in the clear so a presented token
// is one indexed lookup, not a scan-and-argon2id over every row.
const secretBytes = 24

// prefixLen is how much of a token's secret a listing shows. It is a handle for
// the operator, not a credential: six base64url characters name the row a
// human is looking at without printing enough to guess the rest.
const prefixLen = 6

// argon2id parameters. Tuned for an interactive check on a homelab box: one
// pass over 64 MiB across four lanes is tens of milliseconds, which a login
// pays once per session bootstrap and a bearer request pays per call.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrMalformedToken reports a token string that is not scheme_id_secret.
var ErrMalformedToken = errors.New("web: token is malformed")

// newSecret returns a fresh token secret, base64url with no padding.
func newSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("web: read entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// prefixOf is the non-secret handle a listing shows for a secret.
func prefixOf(secret string) string {
	if len(secret) <= prefixLen {
		return secret
	}
	return secret[:prefixLen]
}

// formatToken is the string handed to the operator exactly once. It carries the
// row id so a presented token resolves to one row without argon2id-comparing
// against the whole table.
func formatToken(id int64, secret string) string {
	return fmt.Sprintf("%s_%d_%s", tokenScheme, id, secret)
}

// parseToken splits a presented token into its row id and secret.
func parseToken(token string) (id int64, secret string, err error) {
	parts := strings.SplitN(token, "_", 3)
	if len(parts) != 3 || parts[0] != tokenScheme || parts[2] == "" {
		return 0, "", ErrMalformedToken
	}
	id, perr := strconv.ParseInt(parts[1], 10, 64)
	if perr != nil || id <= 0 {
		return 0, "", ErrMalformedToken
	}
	return id, parts[2], nil
}

// hashSecret returns the argon2id PHC string stored in the token row. The salt
// is fresh per secret, so two tokens with the same bytes still hash apart.
func hashSecret(secret string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("web: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifySecret checks a presented secret against a stored PHC string in
// constant time over the digest. A malformed stored string is an error, never
// a silent pass.
func verifySecret(secret, phc string) (bool, error) {
	fields := strings.Split(phc, "$")
	if len(fields) != 6 || fields[1] != "argon2id" {
		return false, fmt.Errorf("web: %w", errBadPHC)
	}
	var version int
	if _, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, fmt.Errorf("web: %w", errBadPHC)
	}
	var memory, time, threads int
	if _, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, fmt.Errorf("web: %w", errBadPHC)
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil {
		return false, fmt.Errorf("web: %w", errBadPHC)
	}
	want, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil {
		return false, fmt.Errorf("web: %w", errBadPHC)
	}
	got := argon2.IDKey([]byte(secret), salt, uint32(time), uint32(memory), uint8(threads), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

var errBadPHC = errors.New("stored token hash is not a valid argon2id string")
