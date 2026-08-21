package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
			return fmt.Errorf("backup: %s: sha256 %s, manifest says %s", want.Name, got.SHA256, want.SHA256)
		}
		if got.Size != want.Size {
			return fmt.Errorf("backup: %s: %d bytes, manifest says %d", want.Name, got.Size, want.Size)
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
