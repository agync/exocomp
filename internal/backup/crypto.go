package backup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"

	"filippo.io/age"
	"filippo.io/age/armor"

	"exocomp.dev/exocomp/internal/secrets"
)

const dekLen = 32

// armorHeader marks an ASCII-armored age file. keyring.age is armored so it
// survives being pasted around; the git leg's archive is not.
const armorHeader = "-----BEGIN AGE ENCRYPTED FILE-----"

// ResticPassword derives a target's restic repository password from the DEK:
// HKDF-SHA256 over the DEK with the envelope salt and info
// "restic-repo:<target>", base64 of the resulting 32 bytes.
//
// It reproduces (*secrets.Envelope).Derive without an envelope, because the
// host restoring a backup has no host identity to open one with — it has the
// printed recovery key and keyring.age, and nothing else.
// TestResticPasswordMatchesEnvelope in engine_test.go pins this function
// against Derive so the two cannot drift.
func ResticPassword(dek []byte, target string) (string, error) {
	if len(dek) != dekLen {
		return "", fmt.Errorf("backup: dek is %d bytes, want %d", len(dek), dekLen)
	}
	key, err := hkdf.Key(sha256.New, dek, []byte(secrets.Salt), "restic-repo:"+target, dekLen)
	if err != nil {
		return "", fmt.Errorf("backup: derive restic password: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// ManifestMACPurpose is the HKDF purpose the manifest authentication subkey is
// derived under, one key per target. It is on the recovery card's purpose
// table; TestManifestMACPurposeIsOnTheCard pins the two together.
const ManifestMACPurpose = "backup-manifest-mac"

// ManifestMACKey derives a target's manifest authentication key from the DEK.
// Same reason as ResticPassword: the host verifying a manifest during a
// restore has the printed recovery key and keyring.age, and no envelope.
func ManifestMACKey(dek []byte, target string) ([]byte, error) {
	if len(dek) != dekLen {
		return nil, fmt.Errorf("backup: dek is %d bytes, want %d", len(dek), dekLen)
	}
	key, err := hkdf.Key(sha256.New, dek, []byte(secrets.Salt), ManifestMACPurpose+":"+target, dekLen)
	if err != nil {
		return nil, fmt.Errorf("backup: derive manifest mac key: %w", err)
	}
	return key, nil
}

// OpenSealed decrypts a secret row with a raw DEK: AES-256-GCM, binding
// (name, kind) with secrets.AdditionalData. Same reason as ResticPassword —
// after a restore there is no envelope yet, and this is the off-the-shelf path
// the recovery card describes. It shares the envelope's AAD encoder so the two
// cannot drift, and falls back to the legacy encoding so a row sealed before
// E3 still restores.
func OpenSealed(dek []byte, name, kind string, nonce, ciphertext []byte) ([]byte, error) {
	if len(dek) != dekLen {
		return nil, fmt.Errorf("backup: dek is %d bytes, want %d", len(dek), dekLen)
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("backup: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("backup: gcm: %w", err)
	}
	pt, err := aead.Open(nil, nonce, ciphertext, secrets.AdditionalData(name, kind))
	if err == nil {
		return pt, nil
	}
	if pt, legacyErr := aead.Open(nil, nonce, ciphertext, secrets.LegacyAdditionalData(name, kind)); legacyErr == nil {
		return pt, nil
	}
	return nil, fmt.Errorf("backup: open sealed secret %q: %w", name, err)
}

// ReadIdentities parses an age key file.
func ReadIdentities(path string) ([]age.Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("backup: read identity: %w", err)
	}
	defer func() { _ = f.Close() }()

	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("backup: parse identity %s: %w", path, err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("backup: identity file %s holds no identities", path)
	}
	return ids, nil
}

// UnwrapDEK decrypts keyring.age bytes to the 32-byte data encryption key.
func UnwrapDEK(keyring []byte, ids []age.Identity) ([]byte, error) {
	dek, err := ageDecrypt(keyring, ids)
	if err != nil {
		return nil, fmt.Errorf("backup: unwrap dek: %w", err)
	}
	if len(dek) != dekLen {
		return nil, fmt.Errorf("backup: keyring holds a %d-byte key, want %d", len(dek), dekLen)
	}
	return dek, nil
}

func ageDecrypt(data []byte, ids []age.Identity) ([]byte, error) {
	if len(ids) == 0 {
		return nil, errors.New("backup: no age identities")
	}
	var src io.Reader = bytes.NewReader(data)
	if bytes.HasPrefix(bytes.TrimLeft(data, "\n\r "), []byte(armorHeader)) {
		src = armor.NewReader(src)
	}
	r, err := age.Decrypt(src, ids...)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func ageEncrypt(plaintext []byte, recipients []age.Recipient) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipients...)
	if err != nil {
		return nil, fmt.Errorf("backup: age encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("backup: age encrypt: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("backup: age encrypt: %w", err)
	}
	return buf.Bytes(), nil
}
