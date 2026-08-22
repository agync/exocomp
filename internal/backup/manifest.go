package backup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/version"
)

// Names of the files in the staging tree, and what the snapshot becomes once
// it is restored into a data dir.
const (
	ManifestName   = "MANIFEST.json"
	SnapshotName   = "snapshot.db"
	KeyringName    = "keyring.age"
	RecoveryName   = "RECOVERY.md"
	RestoredDBName = "exocomp.db"
)

// manifestVersion is the version of the MANIFEST.json layout itself, bumped
// when a field changes meaning. It is not the database schema version.
const manifestVersion = 1

// ManifestMACName is the file carrying a manifest's authentication tag. It is
// written per target, because the key is derived per target, and it is not
// listed in the manifest it authenticates.
const ManifestMACName = "MANIFEST.mac"

// macAlg names the construction in the tag file, so a reader is never left
// guessing which one produced the hex.
const macAlg = "HMAC-SHA256"

// macSuffix names a tag beside the file it authenticates.
const macSuffix = ".mac"

// ErrManifestTampered reports a manifest, or a parts manifest, that does not
// authenticate under the target's subkey. It is distinct from a hash mismatch:
// a hash mismatch says the bytes rotted, this says they were written by
// something that did not hold the DEK.
var ErrManifestTampered = errors.New("backup: manifest does not authenticate")

// MACFile is what a .mac file holds. Target is informational — the key that
// verifies the tag is already bound to the target it was derived for.
type MACFile struct {
	Alg     string `json:"alg"`
	Purpose string `json:"purpose"`
	Target  string `json:"target"`
	Over    string `json:"over"`
	MAC     string `json:"mac"`
}

// WriteMACFile authenticates body with key and writes the tag beside it.
func WriteMACFile(path string, key, body []byte, target, over string) error {
	tag := MACFile{
		Alg:     macAlg,
		Purpose: ManifestMACPurpose,
		Target:  target,
		Over:    over,
		MAC:     macTag(key, body),
	}
	encoded, err := json.MarshalIndent(tag, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: marshal %s: %w", filepath.Base(path), err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("backup: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// VerifyMACFile checks body against the tag at path. It reports whether a tag
// was there at all: a backup written before manifests were authenticated has
// none, and that is the caller's decision to warn about, not an error.
func VerifyMACFile(path string, key, body []byte, target string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("backup: read %s: %w", filepath.Base(path), err)
	}

	var tag MACFile
	if err := json.Unmarshal(raw, &tag); err != nil {
		return true, fmt.Errorf("%w: %s is not readable: %w", ErrManifestTampered, filepath.Base(path), err)
	}
	if tag.Alg != macAlg {
		return true, fmt.Errorf("%w: %s names algorithm %q, want %s",
			ErrManifestTampered, filepath.Base(path), tag.Alg, macAlg)
	}
	if tag.Target != target {
		return true, fmt.Errorf("%w: %s was written for target %q, restoring as %q",
			ErrManifestTampered, filepath.Base(path), tag.Target, target)
	}
	want, err := hex.DecodeString(tag.MAC)
	if err != nil {
		return true, fmt.Errorf("%w: %s holds an unreadable tag", ErrManifestTampered, filepath.Base(path))
	}
	got, err := hex.DecodeString(macTag(key, body))
	if err != nil {
		return true, fmt.Errorf("backup: %s: %w", filepath.Base(path), err)
	}
	if !hmac.Equal(got, want) {
		return true, fmt.Errorf("%w: %s does not match %s under the %s:%s subkey",
			ErrManifestTampered, tag.Over, filepath.Base(path), ManifestMACPurpose, target)
	}
	return true, nil
}

// AuthenticateManifest tags dir's MANIFEST.json with target's subkey.
func AuthenticateManifest(dir string, key []byte, target string) error {
	body, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return fmt.Errorf("backup: read manifest: %w", err)
	}
	return WriteMACFile(filepath.Join(dir, ManifestMACName), key, body, target, ManifestName)
}

// VerifyManifestMAC checks dir's MANIFEST.json against the tag beside it,
// reporting whether the snapshot carried one.
func VerifyManifestMAC(dir string, key []byte, target string) (bool, error) {
	body, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return false, fmt.Errorf("backup: read manifest: %w", err)
	}
	return VerifyMACFile(filepath.Join(dir, ManifestMACName), key, body, target)
}

func macTag(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// FileEntry is one staged file and the digest a restore checks it against.
type FileEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes one snapshot: what produced it, when, and the digest of
// every file beside it.
type Manifest struct {
	ManifestVersion int               `json:"manifest_version"`
	SchemaVersion   int               `json:"schema_version"`
	ExocompVersion  string            `json:"exocomp_version"`
	CreatedAt       time.Time         `json:"created_at"`
	ToolVersions    map[string]string `json:"tool_versions"`
	Files           []FileEntry       `json:"files"`
}

// WriteManifest hashes every file in dir except the manifest itself and writes
// MANIFEST.json.
func WriteManifest(dir string, m Manifest) (Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: read staging dir: %w", err)
	}

	m.ManifestVersion = manifestVersion
	m.ExocompVersion = version.Version
	m.Files = nil
	for _, e := range entries {
		if e.IsDir() || e.Name() == ManifestName {
			continue
		}
		entry, err := hashFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return Manifest{}, err
		}
		m.Files = append(m.Files, entry)
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Name < m.Files[j].Name })

	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: marshal manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), append(body, '\n'), 0o600); err != nil {
		return Manifest{}, fmt.Errorf("backup: write manifest: %w", err)
	}
	return m, nil
}

// ReadManifest loads MANIFEST.json from dir.
func ReadManifest(dir string) (Manifest, error) {
	body, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, fmt.Errorf("backup: parse manifest: %w", err)
	}
	if m.ManifestVersion > manifestVersion {
		return Manifest{}, fmt.Errorf("backup: manifest version %d is newer than this binary's %d",
			m.ManifestVersion, manifestVersion)
	}
	return m, nil
}

// Verify re-hashes every file the manifest lists in dir. A snapshot that does
// not verify is not restored: a truncated database that opens is worse than a
// restore that stops.
func (m Manifest) Verify(dir string) error {
	if len(m.Files) == 0 {
		return fmt.Errorf("backup: manifest lists no files")
	}
	for _, want := range m.Files {
		got, err := hashFile(filepath.Join(dir, want.Name))
		if err != nil {
			return err
		}
		if got.SHA256 != want.SHA256 {
			return fmt.Errorf("backup: %s is corrupt: sha256 %s, the manifest says %s",
				want.Name, got.SHA256, want.SHA256)
		}
		if got.Size != want.Size {
			return fmt.Errorf("backup: %s is corrupt: %d bytes, the manifest says %d",
				want.Name, got.Size, want.Size)
		}
	}
	return nil
}

func hashFile(path string) (FileEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileEntry{}, fmt.Errorf("backup: hash %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return FileEntry{}, fmt.Errorf("backup: hash %s: %w", filepath.Base(path), err)
	}
	return FileEntry{Name: filepath.Base(path), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// toolVersions probes the tools a restore needs. A missing tool is recorded as
// "missing" rather than failing the run: the leg that needs it fails on its
// own, and the other legs still deserve a manifest.
func toolVersions() map[string]string {
	probes := []struct {
		name string
		args []string
	}{
		{"restic", []string{"version"}},
		{"age", []string{"--version"}},
		{"git", []string{"--version"}},
		{"zstd", []string{"--version"}},
	}

	out := make(map[string]string, len(probes))
	for _, p := range probes {
		path, err := exec.LookPath(p.name)
		if err != nil {
			out[p.name] = "missing"
			continue
		}
		raw, err := exec.Command(path, p.args...).Output()
		if err != nil {
			out[p.name] = "unknown"
			continue
		}
		line, _, _ := strings.Cut(string(raw), "\n")
		out[p.name] = strings.TrimSpace(line)
	}
	return out
}
