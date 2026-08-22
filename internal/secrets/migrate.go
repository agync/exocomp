package secrets

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
)

// hostLabel and recoveryLabel are the two recipients every keyring keeps: the
// identity the daemon opens with, and the one printed on the card.
const (
	hostLabel     = "host"
	recoveryLabel = "recovery"
)

// MigrateOptions describes a change to who can unwrap the DEK, or to where the
// host identity is stored. The DEK itself never changes: re-wrapping is O(1)
// and leaves every sealed secret and every derived subkey alone (ADR 0003).
type MigrateOptions struct {
	// Add takes "age1…" or "label=age1…" recipients.
	Add []string
	// Drop takes a label or a public key. The host and recovery recipients
	// cannot be dropped: one is how the daemon opens the keyring, the other is
	// the printed card.
	Drop []string
	// RotateHost mints a new host identity, wraps the DEK to it, and replaces
	// host.key. The old file is overwritten, not kept.
	RotateHost bool
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// MigrateResult reports what a migration changed. The card is stale from the
// moment it succeeds, which is what Nag says.
type MigrateResult struct {
	From        KeyringVersion
	To          KeyringVersion
	Recipients  []Recipient
	Added       []Recipient
	Dropped     []Recipient
	HostRotated bool
}

// Nag is the line the operator has to act on: a keyring generation that no
// printed page matches.
func (r MigrateResult) Nag() string {
	return fmt.Sprintf("keyring is now %s, replacing %s: run `exocomp recovery-kit`, "+
		"print the new card, and destroy the %s page", r.To, r.From, r.From)
}

// Migrate re-wraps the keyring for a new recipient set, a new host identity, or
// both, and bumps the keyring generation.
//
// The recovery identity stays a recipient throughout, so a crash between the
// renames below leaves a data dir the printed card still recovers.
func Migrate(dataDir string, opts MigrateOptions) (MigrateResult, error) {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if len(opts.Add) == 0 && len(opts.Drop) == 0 && !opts.RotateHost {
		return MigrateResult{}, errors.New("secrets: migrate: nothing to change")
	}

	env, err := Open(dataDir)
	if err != nil {
		return MigrateResult{}, err
	}
	defer func() { _ = env.Close() }()

	res := MigrateResult{From: env.version, HostRotated: opts.RotateHost}
	keep, dropped, err := dropRecipients(env.Recipients(), opts.Drop)
	if err != nil {
		return MigrateResult{}, err
	}
	res.Dropped = dropped

	var host *age.X25519Identity
	if opts.RotateHost {
		host, err = age.GenerateX25519Identity()
		if err != nil {
			return MigrateResult{}, fmt.Errorf("secrets: migrate: generate host identity: %w", err)
		}
		for i := range keep {
			if keep[i].Label == hostLabel {
				keep[i] = newRecipient(hostLabel, host.Recipient().String())
			}
		}
	}

	added, err := parseAdditions(keep, opts.Add)
	if err != nil {
		return MigrateResult{}, err
	}
	res.Added = added
	keep = append(keep, added...)

	if len(keep) < 2 {
		return MigrateResult{}, errors.New("secrets: migrate: a keyring needs at least two recipients")
	}
	wrapTo, err := ageRecipients(keep)
	if err != nil {
		return MigrateResult{}, err
	}
	keyring, err := wrapDEK(env.dek, wrapTo)
	if err != nil {
		return MigrateResult{}, err
	}

	if host != nil {
		if err := replaceHostKey(dataDir, host); err != nil {
			return MigrateResult{}, err
		}
	}
	if err := writeFileAtomic(filepath.Join(dataDir, keyringFile), keyring, 0o644); err != nil {
		return MigrateResult{}, fmt.Errorf("secrets: migrate: write keyring: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dataDir, recipientsFile), marshalRecipients(keep), 0o644); err != nil {
		return MigrateResult{}, fmt.Errorf("secrets: migrate: write recipients: %w", err)
	}

	res.To = KeyringVersion{Version: res.From.Version + 1, Generated: now().UTC().Truncate(time.Second)}
	if err := writeKeyringVersion(dataDir, res.To); err != nil {
		return MigrateResult{}, err
	}
	res.Recipients = keep
	return res, nil
}

// dropRecipients removes the named recipients, refusing to drop the two the
// system cannot work without and refusing a name that matches nothing — a
// mistyped drop that silently changed nothing would be reported as a
// successful migration.
func dropRecipients(current []Recipient, drop []string) (keep, dropped []Recipient, err error) {
	remove := make(map[string]bool, len(drop))
	for _, d := range drop {
		d = strings.TrimSpace(d)
		if d == hostLabel || d == recoveryLabel {
			return nil, nil, fmt.Errorf("secrets: migrate: the %s recipient cannot be dropped", d)
		}
		remove[d] = true
	}

	for _, r := range current {
		switch {
		case remove[r.Label] || remove[r.Public]:
			if r.Label == hostLabel || r.Label == recoveryLabel {
				return nil, nil, fmt.Errorf("secrets: migrate: the %s recipient cannot be dropped", r.Label)
			}
			dropped = append(dropped, r)
			delete(remove, r.Label)
			delete(remove, r.Public)
		default:
			keep = append(keep, r)
		}
	}
	if len(remove) > 0 {
		unknown := slices.Sorted(maps.Keys(remove))
		return nil, nil, fmt.Errorf("secrets: migrate: no such recipient: %s", strings.Join(unknown, ", "))
	}
	return keep, dropped, nil
}

// parseAdditions turns the "label=age1…" forms into recipients, refusing a
// public key the keyring already wraps to.
func parseAdditions(current []Recipient, add []string) ([]Recipient, error) {
	var out []Recipient
	for i, spec := range add {
		label, public, ok := strings.Cut(strings.TrimSpace(spec), "=")
		if !ok {
			public = label
			label = fmt.Sprintf("extra-%d", nextExtra(current, out))
		}
		parsed, err := age.ParseX25519Recipient(strings.TrimSpace(public))
		if err != nil {
			return nil, fmt.Errorf("secrets: migrate: recipient %d: %w", i+1, err)
		}
		r := newRecipient(strings.TrimSpace(label), parsed.String())
		for _, have := range append(slices.Clone(current), out...) {
			if have.Public == r.Public {
				return nil, fmt.Errorf("secrets: migrate: %s is already a recipient (%s)", r.Public, have.Label)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func ageRecipients(rs []Recipient) ([]age.Recipient, error) {
	out := make([]age.Recipient, 0, len(rs))
	for _, r := range rs {
		parsed, err := age.ParseX25519Recipient(r.Public)
		if err != nil {
			return nil, fmt.Errorf("secrets: migrate: recipient %s: %w", r.Label, err)
		}
		out = append(out, parsed)
	}
	return out, nil
}

// replaceHostKey writes the new host identity over the old one. The file keeps
// its 0400 mode, so it is written beside and renamed rather than opened for
// writing.
func replaceHostKey(dataDir string, host *age.X25519Identity) error {
	path := filepath.Join(dataDir, hostKeyFile)
	tmp := path + ".new"
	_ = os.Remove(tmp)
	if err := writeHostKey(tmp, host); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("secrets: migrate: replace host key: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, body, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

// nextExtra continues Init's "extra-N" labelling for a recipient added without
// one, counting the labels already taken rather than the recipients present.
func nextExtra(sets ...[]Recipient) int {
	n := 1
	for _, set := range sets {
		for _, r := range set {
			if strings.HasPrefix(r.Label, "extra-") {
				n++
			}
		}
	}
	return n
}
