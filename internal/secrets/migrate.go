package secrets

import (
	"crypto/rand"
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
	// A drop must revoke, and re-wrapping the same DEK does not: the dropped
	// recipient's retained keyring.age keeps unwrapping it (finding E2). Migrate
	// is the O(1) same-DEK re-wrap for adds and host rotations only; a drop goes
	// through RotateDEK, which mints a new DEK and re-encrypts every secret.
	if len(opts.Drop) > 0 {
		return MigrateResult{}, errors.New("secrets: migrate: dropping a recipient must rotate the DEK; use RotateDEK")
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

// keyringNextName is the side file RotateDEK writes the new keyring to before
// it re-encrypts the secret rows. Holding both the old (active) keyring and the
// new one on disk while rows migrate means a crash never strands rows under a
// DEK no keyring wraps: every row is under one of the two, and both are
// recoverable. It is promoted to the active keyring only once every row is
// re-encrypted.
const keyringNextName = keyringFile + ".next"

// Staged siblings of the other files a DEK rotation replaces. Each is written
// before any row moves and renamed onto its live path only once every row is
// re-encrypted, keyring.age.next last of all (see promoteRotation).
const (
	recipientsNextName     = recipientsFile + ".next"
	keyringVersionNextName = KeyringVersionName + ".next"
	hostKeyNextName        = hostKeyFile + ".next"
	hostKeyAgeNextName     = hostKeyAgeFile + ".next"
)

// RotateDEK is revocation that actually revokes (finding E2). It mints a NEW
// DEK, re-wraps the keyring to the kept recipients only (applying opts.Drop,
// opts.Add and opts.RotateHost), and bumps the generation — so a dropped
// recipient's retained keyring.age unwraps only the OLD, now-unused DEK and
// decrypts nothing that survives the rotation.
//
// reseal is called with the old and new envelopes after the new DEK is minted
// and safely on disk, but before it becomes active: it must re-encrypt every
// sealed row under newEnv and persist it, returning an error to leave the
// rotation to be retried. Derived subkeys (restic repo passwords, tofu state
// passphrases) change with the DEK; RotateDEK does not re-key those consumers —
// the caller must, and must tell the operator so.
func RotateDEK(dataDir string, opts MigrateOptions, reseal func(oldEnv, newEnv *Envelope) error) (MigrateResult, error) {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	// A rotation left half-applied by an earlier crash must be finished, not
	// stacked on: its staged keyring holds a DEK that some rows already moved onto.
	if ResealPending(dataDir) {
		return MigrateResult{}, errResealPending
	}

	oldEnv, err := Open(dataDir)
	if err != nil {
		return MigrateResult{}, err
	}
	defer func() { _ = oldEnv.Close() }()

	res := MigrateResult{From: oldEnv.version, HostRotated: opts.RotateHost}
	keep, dropped, err := dropRecipients(oldEnv.Recipients(), opts.Drop)
	if err != nil {
		return MigrateResult{}, err
	}
	res.Dropped = dropped

	var host *age.X25519Identity
	if opts.RotateHost {
		host, err = age.GenerateX25519Identity()
		if err != nil {
			return MigrateResult{}, fmt.Errorf("secrets: rotate: generate host identity: %w", err)
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
		return MigrateResult{}, errors.New("secrets: rotate: a keyring needs at least two recipients")
	}
	wrapTo, err := ageRecipients(keep)
	if err != nil {
		return MigrateResult{}, err
	}

	newDEK := make([]byte, dekLen)
	if _, err := rand.Read(newDEK); err != nil {
		return MigrateResult{}, fmt.Errorf("secrets: rotate: generate dek: %w", err)
	}
	newKeyring, err := wrapDEK(newDEK, wrapTo)
	if err != nil {
		clear(newDEK)
		return MigrateResult{}, err
	}

	newEnv := &Envelope{
		dataDir:     dataDir,
		dek:         newDEK,
		keyring:     newKeyring,
		host:        oldEnv.host,
		recipients:  keep,
		version:     KeyringVersion{Version: res.From.Version + 1, Generated: now().UTC().Truncate(time.Second)},
		hostKeyProt: oldEnv.hostKeyProt,
	}
	newEnv.lockDEK()
	defer func() { _ = newEnv.Close() }()
	res.To = newEnv.version

	// Stage every file the rotation replaces beside its live copy BEFORE any row
	// moves. keyring.age.next carries the new DEK; recipients.next and the version
	// carry the new public metadata; a staged host key rides along when the host
	// is rotated. The live files are untouched, so the old keyring still opens
	// every not-yet-moved row, and a crash anywhere below leaves keyring.age.next
	// on disk — the marker Reconcile keys on to finish the migration.
	if host != nil {
		if err := stageHostKey(dataDir, host); err != nil {
			return MigrateResult{}, err
		}
	}
	if err := stageRotation(dataDir, newKeyring, keep, res.To); err != nil {
		return MigrateResult{}, err
	}

	if err := reseal(oldEnv, newEnv); err != nil {
		// The staged files stay: rows that already moved onto the new DEK are
		// recoverable through keyring.age.next, and Reconcile finishes the
		// migration on the next Open. Do NOT unstage — that is the data-loss path.
		return MigrateResult{}, fmt.Errorf("secrets: rotate: re-encrypt secrets: %w", err)
	}

	// Every row is now under the new DEK. Promote the staged files atomically,
	// keyring.age last so its rename is the single commit that makes the new DEK
	// active and clears the reconciliation marker.
	if err := promoteRotation(dataDir); err != nil {
		return MigrateResult{}, err
	}
	res.Recipients = keep
	return res, nil
}

// errResealPending refuses to start a fresh rotation on top of an interrupted
// one. The staged keyring holds a DEK that some rows have already moved onto;
// overwriting it would strand those rows under a key nothing wraps. The caller
// reconciles first, then retries.
var errResealPending = errors.New(
	"secrets: rotate: a previous DEK rotation was interrupted; reconcile it before starting another")

// ResealPending reports whether an interrupted DEK rotation left its staged
// keyring on disk. keyring.age.next is promoted last, so its presence is the one
// signal that a rotation did not finish. It is cheap enough to check on every
// Open.
func ResealPending(dataDir string) bool {
	return fileExists(filepath.Join(dataDir, keyringNextName))
}

// Reconcile finishes a DEK rotation that a crash left half-applied. With nothing
// staged it is a no-op, so it is safe to call on every Open. With a staged
// keyring it loads both DEKs — the new one from keyring.age.next, the old one
// from keyring.age while that is still openable — hands them to reseal to
// re-encrypt every row under the new DEK, then atomically promotes the staged
// files.
//
// It is idempotent: a crash during reconciliation converges on the next call.
// reseal decrypts each row under whichever DEK holds it (so a row already moved
// is re-sealed from the new DEK, not lost), and the promote renames are all
// present-or-skip. reseal must, for each stored row, open it under whichever of
// the two envelopes decrypts it, re-seal under newEnv, and persist. oldEnv may be
// nil once keyring.age no longer opens (the host key was already promoted), by
// which point every row is already under the new DEK and only newEnv is needed.
func Reconcile(dataDir string, reseal func(oldEnv, newEnv *Envelope) error) (bool, error) {
	if !ResealPending(dataDir) {
		return false, nil
	}

	newHost, err := loadReconcileHost(dataDir)
	if err != nil {
		return false, err
	}
	newKeyring, err := os.ReadFile(filepath.Join(dataDir, keyringNextName))
	if err != nil {
		return false, fmt.Errorf("secrets: reconcile: read staged keyring: %w", err)
	}
	newDEK, err := unwrapDEK(newKeyring, newHost)
	if err != nil {
		return false, fmt.Errorf("secrets: reconcile: unwrap staged DEK: %w", err)
	}
	newEnv := &Envelope{dataDir: dataDir, dek: newDEK, keyring: newKeyring, host: newHost}
	newEnv.lockDEK()
	defer func() { _ = newEnv.Close() }()

	// Best-effort: keyring.age no longer opens once the host key has been promoted
	// to the new identity, but by then every row is already on the new DEK.
	oldEnv := loadOldEnv(dataDir)
	if oldEnv != nil {
		defer func() { _ = oldEnv.Close() }()
	}

	if err := reseal(oldEnv, newEnv); err != nil {
		return false, fmt.Errorf("secrets: reconcile: re-encrypt secrets: %w", err)
	}
	if err := promoteRotation(dataDir); err != nil {
		return false, err
	}
	return true, nil
}

// stageRotation writes the staged siblings of the files a rotation replaces.
// keyring.age.next is written last so the reconciliation marker never appears
// before the metadata it needs.
func stageRotation(dataDir string, newKeyring []byte, keep []Recipient, version KeyringVersion) error {
	if err := os.WriteFile(filepath.Join(dataDir, recipientsNextName), marshalRecipients(keep), 0o644); err != nil {
		return fmt.Errorf("secrets: rotate: stage recipients: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, keyringVersionNextName), MarshalKeyringVersion(version), 0o644); err != nil {
		return fmt.Errorf("secrets: rotate: stage version: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, keyringNextName), newKeyring, 0o644); err != nil {
		return fmt.Errorf("secrets: rotate: stage keyring: %w", err)
	}
	return nil
}

// stageHostKey writes the new host identity to its staged sibling in the same
// at-rest tier the data dir already uses, leaving the live host key in place so
// the old keyring still opens during the reseal. A systemd-credential host key is
// provisioned outside this process and is refused, as it is for a same-DEK
// migration.
func stageHostKey(dataDir string, host *age.X25519Identity) error {
	if _, ok := credentialHostKeyPath(); ok {
		return fmt.Errorf("secrets: rotate: host key is delivered by a systemd credential; " +
			"rotate it by re-encrypting the credential (systemd-creds encrypt --with-key=host), not in-process")
	}
	passphrase, hasPassphrase, perr := hostKeyPassphrase()
	if perr != nil {
		return perr
	}
	if hasPassphrase {
		tmp := filepath.Join(dataDir, hostKeyAgeNextName)
		_ = os.Remove(tmp)
		return writeWrappedHostKey(tmp, host, passphrase)
	}
	tmp := filepath.Join(dataDir, hostKeyNextName)
	_ = os.Remove(tmp)
	return writeHostKey(tmp, host)
}

// promoteRotation renames every staged sibling onto its live path. The host key
// goes first so the new keyring — wrapped to the new host — can be opened the
// moment it is active; keyring.age goes last so its rename is the single commit
// point. Each earlier rename is present-or-skip, so a crash mid-promote converges
// when Reconcile calls this again.
func promoteRotation(dataDir string) error {
	if err := renameIfPresent(filepath.Join(dataDir, hostKeyNextName), filepath.Join(dataDir, hostKeyFile)); err != nil {
		return fmt.Errorf("secrets: rotate: activate host key: %w", err)
	}
	if err := renameIfPresent(filepath.Join(dataDir, hostKeyAgeNextName), filepath.Join(dataDir, hostKeyAgeFile)); err != nil {
		return fmt.Errorf("secrets: rotate: activate host key: %w", err)
	}
	if err := renameIfPresent(filepath.Join(dataDir, recipientsNextName), filepath.Join(dataDir, recipientsFile)); err != nil {
		return fmt.Errorf("secrets: rotate: activate recipients: %w", err)
	}
	if err := renameIfPresent(filepath.Join(dataDir, keyringVersionNextName), filepath.Join(dataDir, KeyringVersionName)); err != nil {
		return fmt.Errorf("secrets: rotate: activate version: %w", err)
	}
	if err := os.Rename(filepath.Join(dataDir, keyringNextName), filepath.Join(dataDir, keyringFile)); err != nil {
		return fmt.Errorf("secrets: rotate: activate keyring: %w", err)
	}
	return nil
}

// loadReconcileHost resolves the identity that unwraps keyring.age.next. A staged
// host key means the host is being rotated and the new keyring is wrapped to the
// new identity, which lives only in the staged file until promote; otherwise the
// live host key wraps the new keyring too.
func loadReconcileHost(dataDir string) (age.Identity, error) {
	if p := filepath.Join(dataDir, hostKeyAgeNextName); fileExists(p) {
		passphrase, ok, perr := hostKeyPassphrase()
		if perr != nil {
			return nil, perr
		}
		if !ok {
			return nil, fmt.Errorf("secrets: reconcile: staged host key is passphrase-wrapped but no passphrase is set")
		}
		return decryptWrappedHostKey(p, passphrase)
	}
	if p := filepath.Join(dataDir, hostKeyNextName); fileExists(p) {
		return parseHostKeyFile(p)
	}
	id, _, err := loadHostIdentity(dataDir)
	if err != nil {
		return nil, err
	}
	return id, nil
}

// loadOldEnv opens the live keyring under the live host key, returning nil rather
// than an error: the old DEK is only an optimisation for reconciliation, and its
// absence means the rotation is already past the point where any row still needs
// it.
func loadOldEnv(dataDir string) *Envelope {
	host, _, err := loadHostIdentity(dataDir)
	if err != nil {
		return nil
	}
	keyring, err := os.ReadFile(filepath.Join(dataDir, keyringFile))
	if err != nil {
		return nil
	}
	dek, err := unwrapDEK(keyring, host)
	if err != nil {
		return nil
	}
	env := &Envelope{dataDir: dataDir, dek: dek, keyring: keyring, host: host}
	env.lockDEK()
	return env
}

func renameIfPresent(src, dst string) error {
	err := os.Rename(src, dst)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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

// replaceHostKey writes the new host identity over the old one, in the same
// at-rest tier the data dir already uses so a rotation never downgrades a
// wrapped key to plaintext (finding E1). A systemd credential is provisioned
// outside this process, so rotating under it is refused with a pointer to the
// re-provision step rather than silently writing a plaintext key beside it.
func replaceHostKey(dataDir string, host *age.X25519Identity) error {
	if _, ok := credentialHostKeyPath(); ok {
		return fmt.Errorf("secrets: migrate: host key is delivered by a systemd credential; " +
			"rotate it by re-encrypting the credential (systemd-creds encrypt --with-key=host), not in-process")
	}

	passphrase, hasPassphrase, perr := hostKeyPassphrase()
	if perr != nil {
		return perr
	}
	if hasPassphrase {
		path := filepath.Join(dataDir, hostKeyAgeFile)
		tmp := path + ".new"
		_ = os.Remove(tmp)
		if err := writeWrappedHostKey(tmp, host, passphrase); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("secrets: migrate: replace host key: %w", err)
		}
		return nil
	}

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
