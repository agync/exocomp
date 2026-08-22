package secrets

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
)

// SealedSuffix marks a file encrypted at rest to the keyring's recipients.
// The name is the marker: a caller reads the suffix, not a database column, so
// a file that outlives its row still says what it is.
const SealedSuffix = ".age"

// SealFile encrypts path to path+".age" for every keyring recipient and removes
// the plaintext, returning the sealed path. It is for artifacts a job produced
// that hold values the database never would — an OpenTofu plan above all, which
// carries every attribute of every resource it would change, in the clear.
//
// A file already carrying the suffix is left alone: sealing twice would leave a
// plan nothing can read without two passes.
func (e *Envelope) SealFile(path string) (string, error) {
	if strings.HasSuffix(path, SealedSuffix) {
		return path, nil
	}
	plaintext, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("secrets: seal %s: %w", path, err)
	}

	recipients, err := ageRecipients(e.recipients)
	if err != nil {
		return "", err
	}
	if len(recipients) == 0 {
		return "", fmt.Errorf("secrets: seal %s: the keyring has no recipients", path)
	}

	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipients...)
	if err != nil {
		return "", fmt.Errorf("secrets: seal %s: %w", path, err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return "", fmt.Errorf("secrets: seal %s: %w", path, err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("secrets: seal %s: %w", path, err)
	}

	sealed := path + SealedSuffix
	if err := os.WriteFile(sealed, buf.Bytes(), 0o600); err != nil {
		return "", fmt.Errorf("secrets: seal %s: %w", path, err)
	}
	// Only once the ciphertext is on disk: a crash between the two leaves the
	// artifact readable, never lost.
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("secrets: seal %s: remove plaintext: %w", path, err)
	}
	return sealed, nil
}

// OpenSealedFile decrypts a file SealFile wrote, with the host identity this
// envelope was opened from. A file with no suffix is returned as it is: an
// artifact written before sealing existed is still an artifact.
func (e *Envelope) OpenSealedFile(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: open %s: %w", path, err)
	}
	if !strings.HasSuffix(path, SealedSuffix) {
		return body, nil
	}
	if e.host == nil {
		return nil, fmt.Errorf("secrets: open %s: this envelope holds no host identity", path)
	}

	r, err := age.Decrypt(bytes.NewReader(body), e.host)
	if err != nil {
		return nil, fmt.Errorf("secrets: open %s: %w", path, err)
	}
	plaintext, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("secrets: open %s: %w", path, err)
	}
	return plaintext, nil
}
