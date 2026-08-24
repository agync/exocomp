package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// Salt is the HKDF domain-separation salt for this envelope version. It is
// printed on the recovery kit, so changing it invalidates every printed card.
const Salt = "exocomp/v1"

// KeyringName is the file wrapping the DEK. Backup targets carry a copy of it
// at their root, outside the repository.
const KeyringName = "keyring.age"

const (
	hostKeyFile    = "host.key"
	keyringFile    = KeyringName
	recipientsFile = "recipients"

	dekLen   = 32
	nonceLen = 12
)

// ErrAlreadyInitialized reports that the data dir already holds a keyring.
var ErrAlreadyInitialized = errors.New("secrets: keyring already exists")

// ErrClosed reports use of an Envelope whose DEK has been zeroed.
var ErrClosed = errors.New("secrets: envelope closed")

// Envelope holds the unwrapped data encryption key and the public keyring that
// wraps it. The DEK lives in exactly one []byte, zeroed by Close.
type Envelope struct {
	dataDir string
	dek     []byte
	keyring []byte
	// host is the identity the keyring was opened with. It is kept so an
	// artifact sealed to the keyring's recipients can be read back without a
	// second trip to disk for the key.
	host        age.Identity
	recipients  []Recipient
	version     KeyringVersion
	memLock     MemoryLock
	hostKeyProt HostKeyProtection
}

// SealedSecret is one AES-256-GCM ciphertext. Name and Kind are bound as
// additional data, so moving a ciphertext to another row fails to open.
type SealedSecret struct {
	Name       string
	Kind       string
	Nonce      []byte
	Ciphertext []byte
}

// Init creates the host identity and the keyring under dataDir and returns the
// live envelope plus the recovery kit. The recovery identity's secret key is
// returned in the kit and never written to disk.
func Init(dataDir string, extraRecipients []string) (*Envelope, *RecoveryKit, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("secrets: create data dir: %w", err)
	}
	switch _, err := os.Stat(filepath.Join(dataDir, keyringFile)); {
	case err == nil:
		return nil, nil, ErrAlreadyInitialized
	case !errors.Is(err, os.ErrNotExist):
		return nil, nil, fmt.Errorf("secrets: stat keyring: %w", err)
	}

	host, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: generate host identity: %w", err)
	}
	recovery, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: generate recovery identity: %w", err)
	}

	recipients := []Recipient{
		newRecipient("host", host.Recipient().String()),
		newRecipient("recovery", recovery.Recipient().String()),
	}
	wrapTo := []age.Recipient{host.Recipient(), recovery.Recipient()}
	for i, s := range extraRecipients {
		r, err := age.ParseX25519Recipient(strings.TrimSpace(s))
		if err != nil {
			return nil, nil, fmt.Errorf("secrets: extra recipient %d: %w", i+1, err)
		}
		recipients = append(recipients, newRecipient(fmt.Sprintf("extra-%d", i+1), r.String()))
		wrapTo = append(wrapTo, r)
	}

	dek := make([]byte, dekLen)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, fmt.Errorf("secrets: generate dek: %w", err)
	}
	keyring, err := wrapDEK(dek, wrapTo)
	if err != nil {
		return nil, nil, err
	}

	// The default (non-TPM) story must be genuinely secure: when an
	// operator passphrase is set, the host key is scrypt-wrapped and no
	// plaintext key ever touches the data dir. Without one it is a bare
	// plaintext file — the caller (init/setup) warns loudly and points at the
	// fix, and the protection state below reports it honestly.
	passphrase, hasPassphrase, perr := hostKeyPassphrase()
	if perr != nil {
		return nil, nil, perr
	}
	protection := HostKeyPlaintext
	if hasPassphrase {
		if err := writeWrappedHostKey(filepath.Join(dataDir, hostKeyAgeFile), host, passphrase); err != nil {
			return nil, nil, err
		}
		protection = HostKeyPassphrase
	} else if err := writeHostKey(filepath.Join(dataDir, hostKeyFile), host); err != nil {
		return nil, nil, err
	}
	// A half-written dir would fail every retry on the host key's O_EXCL, so
	// roll back to an uninitialized dir instead.
	if err := os.WriteFile(filepath.Join(dataDir, recipientsFile), marshalRecipients(recipients), 0o644); err != nil {
		Rollback(dataDir)
		return nil, nil, fmt.Errorf("secrets: write recipients: %w", err)
	}
	// Truncated to the second the file stores, so the envelope in memory and
	// the file on disk report the same generation.
	version := KeyringVersion{
		Version:   firstKeyringVersion,
		Generated: time.Now().UTC().Truncate(time.Second),
	}
	if err := writeKeyringVersion(dataDir, version); err != nil {
		Rollback(dataDir)
		return nil, nil, err
	}
	// Written last: the keyring's presence is what marks the dir initialized.
	if err := os.WriteFile(filepath.Join(dataDir, keyringFile), keyring, 0o644); err != nil {
		Rollback(dataDir)
		return nil, nil, fmt.Errorf("secrets: write keyring: %w", err)
	}

	env := &Envelope{
		dataDir:     dataDir,
		dek:         dek,
		keyring:     keyring,
		host:        host,
		recipients:  recipients,
		version:     version,
		hostKeyProt: protection,
	}
	env.lockDEK()
	kit := env.KitStructure()
	kit.AgeSecretKey = recovery.String()
	return env, kit, nil
}

// Rollback removes every file a completed Init writes, restoring dataDir to an
// uninitialized state. Init's own writes are already atomic — this is for a
// caller whose "all or nothing" promise spans further than Init's return: if a
// step it runs after a successful Init fails, the caller undoes the identity
// with this rather than leaving a host that is on-disk-initialized but never
// finished setup. Best-effort: removal errors are ignored, since a missing
// file is already the desired end state.
func Rollback(dataDir string) {
	for _, name := range []string{hostKeyFile, hostKeyAgeFile, recipientsFile, KeyringVersionName, keyringFile} {
		_ = os.Remove(filepath.Join(dataDir, name))
	}
}

// Open loads the host identity from dataDir — from the systemd credential, a
// passphrase-wrapped file, or a plaintext file, in that order — and unwraps the
// DEK from the keyring.
func Open(dataDir string) (*Envelope, error) {
	host, protection, err := loadHostIdentity(dataDir)
	if err != nil {
		return nil, err
	}

	keyring, err := os.ReadFile(filepath.Join(dataDir, keyringFile))
	if err != nil {
		return nil, fmt.Errorf("secrets: read keyring: %w", err)
	}
	dek, err := unwrapDEK(keyring, host)
	if err != nil {
		return nil, err
	}

	recipients, err := readRecipients(filepath.Join(dataDir, recipientsFile))
	if err != nil {
		return nil, err
	}
	version, err := ReadKeyringVersion(dataDir)
	if err != nil {
		return nil, err
	}
	env := &Envelope{
		dataDir:     dataDir,
		dek:         dek,
		keyring:     keyring,
		host:        host,
		recipients:  recipients,
		version:     version,
		hostKeyProt: protection,
	}
	env.lockDEK()
	return env, nil
}

// HostKeyProtection reports how the host identity this envelope opened with is
// protected at rest.
func (e *Envelope) HostKeyProtection() HostKeyProtection { return e.hostKeyProt }

// Seal encrypts pt under the DEK with a fresh 96-bit nonce, binding name|kind.
func (e *Envelope) Seal(name, kind string, pt []byte) (SealedSecret, error) {
	aead, err := e.aead()
	if err != nil {
		return SealedSecret{}, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return SealedSecret{}, fmt.Errorf("secrets: generate nonce: %w", err)
	}
	return SealedSecret{
		Name:       name,
		Kind:       kind,
		Nonce:      nonce,
		Ciphertext: aead.Seal(nil, nonce, pt, additionalData(name, kind)),
	}, nil
}

// aadVersion tags the length-prefixed AAD encoding. A tag byte the legacy
// "name|kind" bytes never begin with (both hostname characters and the pipe
// exclude 0x01) keeps the two encodings disjoint, so OpenSecret can try the
// current encoding first and fall back to the legacy one for a row sealed
// before this change without risking a false match.
const aadVersion byte = 0x01

// OpenSecret decrypts s, failing if the ciphertext, the nonce, or the
// name|kind pair it was sealed under has changed.
func (e *Envelope) OpenSecret(s SealedSecret) ([]byte, error) {
	aead, err := e.aead()
	if err != nil {
		return nil, err
	}
	if len(s.Nonce) != nonceLen {
		return nil, fmt.Errorf("secrets: nonce is %d bytes, want %d", len(s.Nonce), nonceLen)
	}
	pt, err := aead.Open(nil, s.Nonce, s.Ciphertext, additionalData(s.Name, s.Kind))
	if err == nil {
		return pt, nil
	}
	// A row sealed before the AAD became length-prefixed carries the old
	// "name|kind" bytes. Try them so existing rows still open; a fresh seal is
	// always length-prefixed, so this path never widens the collision the new
	// encoding closes.
	if pt, legacyErr := aead.Open(nil, s.Nonce, s.Ciphertext, legacyAdditionalData(s.Name, s.Kind)); legacyErr == nil {
		return pt, nil
	}
	return nil, fmt.Errorf("secrets: open %q: %w", s.Name, err)
}

// Derive returns the 32-byte subkey for purpose:id via HKDF-SHA256 over the
// DEK. It returns nil once the envelope is closed.
func (e *Envelope) Derive(purpose, id string) []byte {
	if len(e.dek) != dekLen {
		return nil
	}
	k, err := DeriveWithSalt(e.dek, Salt, purpose, id)
	if err != nil {
		// Unreachable: HKDF-SHA256 admits outputs up to 255*32 bytes. Panicking
		// beats handing a caller a zero-value repository password.
		panic("secrets: hkdf: " + err.Error())
	}
	return k
}

// DeriveWithSalt derives purpose:id under an explicit salt, for a caller
// reproducing a page printed under a salt this binary no longer defaults to.
func (e *Envelope) DeriveWithSalt(salt, purpose, id string) ([]byte, error) {
	if len(e.dek) != dekLen {
		return nil, ErrClosed
	}
	return DeriveWithSalt(e.dek, salt, purpose, id)
}

// DeriveWithSalt is the card's one rule, over a raw DEK and an explicit salt:
// HKDF-SHA256, info "<purpose>:<id>", 32 bytes out. `exocomp kit derive` runs
// it offline against a DEK unwrapped from keyring.age, where there is no
// envelope to open and the salt comes off the printed page.
func DeriveWithSalt(dek []byte, salt, purpose, id string) ([]byte, error) {
	if len(dek) != dekLen {
		return nil, fmt.Errorf("secrets: dek is %d bytes, want %d", len(dek), dekLen)
	}
	k, err := hkdf.Key(sha256.New, dek, []byte(salt), purpose+":"+id, dekLen)
	if err != nil {
		return nil, fmt.Errorf("secrets: derive %s:%s: %w", purpose, id, err)
	}
	// Best-effort: the derived subkey is a secret too, and is short-lived and
	// zeroed by its caller. A host that forbids mlock still derives; only the
	// DEK's lock state is tracked and reported.
	_ = lockMemory(k)
	return k, nil
}

// KeyringAge returns the armored age ciphertext wrapping the DEK, one recipient
// stanza per keyring recipient. It is ciphertext: safe to publish, and it is
// written to every backup target root outside the restic repository.
func (e *Envelope) KeyringAge() ([]byte, error) {
	if len(e.keyring) == 0 {
		return nil, errors.New("secrets: no keyring loaded")
	}
	return bytes.Clone(e.keyring), nil
}

// Recipients lists the public keyring recipients.
func (e *Envelope) Recipients() []Recipient { return slices.Clone(e.recipients) }

// DataDir reports the directory this envelope was opened from.
func (e *Envelope) DataDir() string { return e.dataDir }

// KeyringVersion reports the generation of the keyring this envelope was
// opened from.
func (e *Envelope) KeyringVersion() KeyringVersion { return e.version }

// KitStructure renders the kit from stored public data only. AgeSecretKey stays
// empty: the recovery secret key exists in memory only at Init or rotation.
func (e *Envelope) KitStructure() *RecoveryKit {
	return &RecoveryKit{
		Version:    e.version,
		Salt:       Salt,
		Recipients: e.Recipients(),
	}
}

// MemoryLock reports whether the DEK is pinned into RAM (out of swap and core
// dumps), and why not when it is not.
func (e *Envelope) MemoryLock() MemoryLock { return e.memLock }

// lockDEK pins the DEK's pages into RAM. A host that forbids it still runs;
// doctor reports the state either way.
func (e *Envelope) lockDEK() {
	if err := lockMemory(e.dek); err != nil {
		e.memLock = MemoryLock{Locked: false, Err: err}
		return
	}
	e.memLock = MemoryLock{Locked: true}
}

// Close zeroes the DEK and releases its memory lock. Every later Seal,
// OpenSecret, or Derive fails.
func (e *Envelope) Close() error {
	if e.memLock.Locked {
		_ = unlockMemory(e.dek)
		e.memLock = MemoryLock{Locked: false}
	}
	clear(e.dek)
	e.dek = nil
	return nil
}

func (e *Envelope) aead() (cipher.AEAD, error) {
	if len(e.dek) != dekLen {
		return nil, ErrClosed
	}
	block, err := aes.NewCipher(e.dek)
	if err != nil {
		return nil, fmt.Errorf("secrets: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}
	return aead, nil
}

// AdditionalData binds (name, kind) into the AEAD unambiguously. Length
// prefixing each field means no two distinct (name, kind) pairs share an
// encoding — ("a", "b|c") and ("a|b", "c") differ here — so a ciphertext moved
// to another row fails to open, the property the "name|kind" concatenation lost
// to its unescaped separator.
//
// It is exported because the offline restore path reproduces the envelope's
// decryption without an envelope (internal/backup), and the two must bind the
// same additional data or a restored row will not open.
func AdditionalData(name, kind string) []byte {
	b := make([]byte, 0, 1+4+len(name)+4+len(kind))
	b = append(b, aadVersion)
	b = binary.BigEndian.AppendUint32(b, uint32(len(name)))
	b = append(b, name...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(kind)))
	b = append(b, kind...)
	return b
}

// LegacyAdditionalData is the pre-E3 encoding, kept only so a row sealed under
// it can still be read. Nothing seals with it any more.
func LegacyAdditionalData(name, kind string) []byte { return []byte(name + "|" + kind) }

func additionalData(name, kind string) []byte       { return AdditionalData(name, kind) }
func legacyAdditionalData(name, kind string) []byte { return LegacyAdditionalData(name, kind) }

func wrapDEK(dek []byte, to []age.Recipient) ([]byte, error) {
	var buf bytes.Buffer
	armored := armor.NewWriter(&buf)
	w, err := age.Encrypt(armored, to...)
	if err != nil {
		return nil, fmt.Errorf("secrets: wrap dek: %w", err)
	}
	if _, err := w.Write(dek); err != nil {
		return nil, fmt.Errorf("secrets: wrap dek: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("secrets: wrap dek: %w", err)
	}
	if err := armored.Close(); err != nil {
		return nil, fmt.Errorf("secrets: armor keyring: %w", err)
	}
	return buf.Bytes(), nil
}

func unwrapDEK(keyring []byte, id age.Identity) ([]byte, error) {
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(keyring)), id)
	if err != nil {
		return nil, fmt.Errorf("secrets: unwrap dek: %w", err)
	}
	dek, err := io.ReadAll(io.LimitReader(r, dekLen+1))
	if err != nil {
		return nil, fmt.Errorf("secrets: unwrap dek: %w", err)
	}
	if len(dek) != dekLen {
		return nil, fmt.Errorf("secrets: keyring holds a %d-byte key, want %d", len(dek), dekLen)
	}
	return dek, nil
}

func writeHostKey(path string, id *age.X25519Identity) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return fmt.Errorf("secrets: create host key: %w", err)
	}
	if _, err := io.WriteString(f, id.String()+"\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("secrets: write host key: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("secrets: write host key: %w", err)
	}
	return nil
}
