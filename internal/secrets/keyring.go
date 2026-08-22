package secrets

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// KeyringVersionName is the file recording which generation of the keyring a
// directory holds. It rides beside keyring.age rather than inside it: age
// rejects any framing added before or after an armored file, and the printed
// card's `age -d -i recovery.key keyring.age` has to keep working verbatim.
const KeyringVersionName = "keyring.version"

// firstKeyringVersion is what a directory written before versioning existed
// holds. Its absence is the version, which is why ReadKeyringVersion reports it
// for a missing file instead of failing.
const firstKeyringVersion = 1

// KeyringVersion is one generation of the keyring: the monotonic counter that
// rotation and recipient changes bump, and when that generation was minted.
// A restore refuses a keyring older than the one its snapshot was written
// under, so this counter is what makes rolling an old keyring back onto a
// target detectable.
type KeyringVersion struct {
	Version   int
	Generated time.Time
}

// String renders the generation the way the card and the CLI name it.
func (v KeyringVersion) String() string { return "v" + strconv.Itoa(v.Version) }

// ReadKeyringVersion reports the generation of the keyring in dir. Both forms
// parse: a directory with no version file is generation 1, the shape every
// keyring written before this existed has.
func ReadKeyringVersion(dir string) (KeyringVersion, error) {
	body, err := os.ReadFile(filepath.Join(dir, KeyringVersionName))
	if errors.Is(err, os.ErrNotExist) {
		return KeyringVersion{Version: firstKeyringVersion}, nil
	}
	if err != nil {
		return KeyringVersion{}, fmt.Errorf("secrets: read keyring version: %w", err)
	}
	return ParseKeyringVersion(body)
}

// ParseKeyringVersion reads a version file's bytes. Backup targets keep the
// same file beside keyring.age, and an s3 target reads it as an object rather
// than off a filesystem.
func ParseKeyringVersion(body []byte) (KeyringVersion, error) {
	out := KeyringVersion{}
	for line := range strings.Lines(string(body)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "version":
			n, err := strconv.Atoi(value)
			if err != nil {
				return KeyringVersion{}, fmt.Errorf("secrets: keyring version %q: %w", value, err)
			}
			out.Version = n
		case "generated":
			ts, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return KeyringVersion{}, fmt.Errorf("secrets: keyring generated %q: %w", value, err)
			}
			out.Generated = ts.UTC()
		}
	}
	if out.Version < firstKeyringVersion {
		return KeyringVersion{}, fmt.Errorf("secrets: %s names no version", KeyringVersionName)
	}
	return out, nil
}

// MarshalKeyringVersion renders the version file's bytes. It is exported
// because the backup legs write the same bytes beside keyring.age at every
// target root.
func MarshalKeyringVersion(v KeyringVersion) []byte {
	generated := v.Generated
	if generated.IsZero() {
		generated = time.Now()
	}
	return []byte("version: " + strconv.Itoa(v.Version) + "\n" +
		"generated: " + generated.UTC().Format(time.RFC3339) + "\n")
}

func writeKeyringVersion(dir string, v KeyringVersion) error {
	if err := os.WriteFile(filepath.Join(dir, KeyringVersionName), MarshalKeyringVersion(v), 0o644); err != nil {
		return fmt.Errorf("secrets: write keyring version: %w", err)
	}
	return nil
}

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
