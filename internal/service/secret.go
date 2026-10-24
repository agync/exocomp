package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// The secret verbs seal a value into the envelope and record it, rotate it, and
// bind it to an env name for runs. The CLI and the /api/v1 secret routes call
// them, so a secret sealed on either face lands in the same row shape. A value
// only ever travels in: no verb here returns one.

// Sealer seals a plaintext under name|kind. *secrets.Envelope implements it.
type Sealer interface {
	Seal(name, kind string, pt []byte) (secrets.SealedSecret, error)
}

// generatedValueBytes is the entropy behind a generated value: 32 random bytes
// (256 bits), rendered base64url so the sealed plaintext is printable env-safe
// ASCII.
const generatedValueBytes = 32

// MaxSecretValueBytes is the sealed-secret value size ceiling.
const MaxSecretValueBytes = 1 << 20

// ErrEmptySecretValue refuses a seal or a user replace with nothing to seal.
var ErrEmptySecretValue = errors.New("empty value")

// GenerateSecretValue mints a cryptographically random secret value.
func GenerateSecretValue() ([]byte, error) {
	raw := make([]byte, generatedValueBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate value: %w", err)
	}
	return []byte(base64.RawURLEncoding.EncodeToString(raw)), nil
}

// SealSecretInput is one new sealed secret: a value, or Generate to mint one.
type SealSecretInput struct {
	Name      string
	Kind      string
	Value     []byte
	Generate  bool
	ExpiresAt *time.Time
}

// SealSecretResult is the recorded row and the size of the sealed value.
type SealSecretResult struct {
	Row   store.SecretRow
	Bytes int
}

// SealSecret seals a value, or a minted one, and records it. A name already in
// use is store.ErrDuplicate.
func (s *Service) SealSecret(ctx context.Context, sealer Sealer, in SealSecretInput) (SealSecretResult, error) {
	value, source := in.Value, store.SecretSourceUser
	if in.Generate {
		var err error
		if value, err = GenerateSecretValue(); err != nil {
			return SealSecretResult{}, err
		}
		source = store.SecretSourceGenerated
	}
	if err := checkSecretValue(value); err != nil {
		return SealSecretResult{}, err
	}
	sealed, err := sealer.Seal(in.Name, in.Kind, value)
	if err != nil {
		return SealSecretResult{}, err
	}
	row, err := s.store.CreateSecret(ctx, store.SecretRow{
		Name:       sealed.Name,
		Kind:       sealed.Kind,
		Nonce:      sealed.Nonce,
		Ciphertext: sealed.Ciphertext,
		AAD:        sealed.Name + "|" + sealed.Kind,
		Source:     source,
		ExpiresAt:  in.ExpiresAt,
	})
	if err != nil {
		return SealSecretResult{}, err
	}
	return SealSecretResult{Row: row, Bytes: len(value)}, nil
}

// ReplaceSecretInput is a new value for an existing secret. A generated secret
// is minted again whatever Value holds; Generate mints one for a user secret
// too. SetExpiry replaces the expiry with ExpiresAt (nil clears it).
type ReplaceSecretInput struct {
	Name      string
	Value     []byte
	Generate  bool
	SetExpiry bool
	ExpiresAt *time.Time
}

// ReplaceSecretResult says what happened: a generated secret is rotated, a
// user secret only replaced (its old value may still be live upstream).
type ReplaceSecretResult struct {
	Name        string
	Rotated     bool
	Regenerated bool
	Bytes       int
	SetExpiry   bool
	ExpiresAt   *time.Time
}

// ReplaceSecret reseals a secret under its unchanged name and kind, so the row
// still opens.
func (s *Service) ReplaceSecret(ctx context.Context, sealer Sealer, in ReplaceSecretInput) (ReplaceSecretResult, error) {
	existing, err := s.store.GetSecret(ctx, in.Name)
	if err != nil {
		return ReplaceSecretResult{}, err
	}
	res := ReplaceSecretResult{
		Name:      existing.Name,
		Rotated:   existing.Source == store.SecretSourceGenerated,
		SetExpiry: in.SetExpiry,
		ExpiresAt: in.ExpiresAt,
	}
	value := in.Value
	if in.Generate || res.Rotated {
		if value, err = GenerateSecretValue(); err != nil {
			return ReplaceSecretResult{}, err
		}
		res.Regenerated = true
	}
	if err := checkSecretValue(value); err != nil {
		return ReplaceSecretResult{}, err
	}
	sealed, err := sealer.Seal(existing.Name, existing.Kind, value)
	if err != nil {
		return ReplaceSecretResult{}, err
	}
	// The value and the expiry land in one statement: an error then means
	// nothing changed, which is what the caller is told.
	if in.SetExpiry {
		err = s.store.RotateSecretSetExpiry(ctx, existing.Name, sealed.Nonce, sealed.Ciphertext, in.ExpiresAt)
	} else {
		err = s.store.RotateSecret(ctx, existing.Name, sealed.Nonce, sealed.Ciphertext)
	}
	if err != nil {
		return ReplaceSecretResult{}, err
	}
	s.BumpKitChanges(ctx)
	res.Bytes = len(value)
	return res, nil
}

func checkSecretValue(value []byte) error {
	if len(value) == 0 {
		return ErrEmptySecretValue
	}
	if len(value) > MaxSecretValueBytes {
		return fmt.Errorf("value exceeds the %d-byte limit", MaxSecretValueBytes)
	}
	return nil
}

// SecretBindings returns every run-secret pair that maps an env name to the
// secret, the guard a delete checks first.
func (s *Service) SecretBindings(ctx context.Context, name string) ([]store.RunSecret, error) {
	bindings, err := s.store.ListRunSecretBindings(ctx)
	if err != nil {
		return nil, err
	}
	var refs []store.RunSecret
	for _, b := range bindings {
		pairs, err := s.store.RunSecretsByBinding(ctx, b)
		if err != nil {
			return nil, err
		}
		for _, p := range pairs {
			if p.SecretName == name {
				refs = append(refs, p)
			}
		}
	}
	return refs, nil
}

// ValidEnvName reports whether s is a POSIX environment variable name.
func ValidEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// IsSystemSecret reports whether a secret is, or would take the place of, one
// exocomp makes for itself rather than one an operator sealed: a git source's
// deploy key (kind "deploy-key", name source/<name>/deploy-key), the fleet ssh
// key (kind "fleet-ssh-key", name fleet/ssh, which `host onboard` reuses), or a
// sealed play or tofu output (kinds "ansible-output" and "tofu-output", names
// under ansible-output/ and tofu-output/). No operator held these values, so
// the API faces neither seal, replace, remove nor bind one (Decision 0027). The
// name is checked as well as the kind, so a secret sealed under a reserved name
// with another kind cannot stand in for one.
func IsSystemSecret(name, kind string) bool {
	switch kind {
	case "deploy-key", "fleet-ssh-key", "ansible-output", "tofu-output":
		return true
	}
	if name == "fleet/ssh" || strings.HasPrefix(name, "ansible-output/") || strings.HasPrefix(name, "tofu-output/") {
		return true
	}
	rest, ok := strings.CutPrefix(name, "source/")
	return ok && strings.HasSuffix(rest, "/deploy-key")
}

// IsSystemBinding reports whether a run-secret binding is one exocomp collects
// sealed outputs under: ab:<workflow> for a play's captures, tf:<workspace> for
// a workspace's outputs (internal/executor). A dependent step resolves it by
// name, so the API faces neither add to nor remove it.
func IsSystemBinding(binding string) bool {
	return strings.HasPrefix(binding, "ab:") || strings.HasPrefix(binding, "tf:")
}

// BumpKitChanges records that a secret changed, which leaves a printed recovery
// kit stale until it is reprinted.
func (s *Service) BumpKitChanges(ctx context.Context) {
	v := 0
	if raw, err := s.store.GetSetting(ctx, store.SettingKitChanges); err == nil {
		if n, perr := strconv.Atoi(raw); perr == nil {
			v = n
		}
	}
	_ = s.store.PutSetting(ctx, store.SettingKitChanges, strconv.Itoa(v+1))
}

// ErrInvalidEnvName refuses a run-secret pair whose env name is not one.
var ErrInvalidEnvName = errors.New("not a valid environment variable name")

// BindRunSecret maps an env name to a sealed secret under a binding. It refuses
// a secret that does not exist, so the failure lands here rather than at run
// time, when a plugin would silently get an empty token.
func (s *Service) BindRunSecret(ctx context.Context, binding, envName, secretName string) (store.RunSecret, error) {
	if !ValidEnvName(envName) {
		return store.RunSecret{}, fmt.Errorf("%q: %w", envName, ErrInvalidEnvName)
	}
	if _, err := s.store.GetSecret(ctx, secretName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.RunSecret{}, fmt.Errorf("no sealed secret %q: %w", secretName, store.ErrNotFound)
		}
		return store.RunSecret{}, err
	}
	return s.store.AddRunSecret(ctx, store.RunSecret{Binding: binding, EnvName: envName, SecretName: secretName})
}
