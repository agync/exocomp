package secrets

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Recipient is one public age recipient that can unwrap the DEK.
type Recipient struct {
	Label       string
	Public      string
	Fingerprint string
}

func newRecipient(label, public string) Recipient {
	return Recipient{Label: label, Public: public, Fingerprint: fingerprint(public)}
}

// fingerprint truncates SHA-256 of the recipient string to 8 bytes, grouped so
// an operator can compare it against a printed card. It identifies a recipient;
// it is not a security boundary.
func fingerprint(public string) string {
	sum := sha256.Sum256([]byte(public))
	h := hex.EncodeToString(sum[:8])
	groups := make([]string, 0, len(h)/4)
	for i := 0; i < len(h); i += 4 {
		groups = append(groups, h[i:i+4])
	}
	return strings.Join(groups, "-")
}

func marshalRecipients(rs []Recipient) []byte {
	lines := []string{"# exocomp keyring recipients - public data, safe to publish"}
	for _, r := range rs {
		lines = append(lines, r.Label+" "+r.Public)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func readRecipients(path string) ([]Recipient, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: read recipients: %w", err)
	}
	defer func() { _ = f.Close() }()

	var out []Recipient
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		label, public, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("secrets: recipients: malformed line %q", line)
		}
		out = append(out, newRecipient(label, strings.TrimSpace(public)))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("secrets: read recipients: %w", err)
	}
	return out, nil
}
