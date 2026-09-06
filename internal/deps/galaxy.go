package deps

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// galaxyOverride and galaxyRoot are where an ansible collections requirements
// file lives: the .exocomp override wins over the repo-root default, mirroring
// the Python manifest precedence.
const (
	galaxyOverride = ".exocomp/requirements.yml"
	galaxyRoot     = "requirements.yml"
)

// GalaxyManifest is a discovered ansible collections requirements file, the
// input to EnsureCollections (internal/deps/collections.go), which installs the
// collections into a per-source cache dir for offline resolution (ADR 0015).
type GalaxyManifest struct {
	// Root is the project root discovery settled on, the namespace the
	// per-source collections cache is keyed by (PythonManifest.Root, mirrored).
	Root string
	// Path is the absolute requirements.yml, empty when none is present.
	Path string
	// Hash is a content hash over the file, empty when none is present.
	Hash string
}

// Found reports whether a collections requirements file was discovered.
func (g GalaxyManifest) Found() bool { return g.Path != "" }

// DiscoverGalaxy resolves the collections requirements file for the project
// rooted at (or above) start: an .exocomp/requirements.yml override, else a
// repo-root requirements.yml. It only locates the file — EnsureCollections
// performs the install — so discovery and provisioning stay one seam.
func DiscoverGalaxy(start string) (GalaxyManifest, error) {
	root := FindProjectRoot(start)
	for _, rel := range []string{galaxyOverride, galaxyRoot} {
		path := filepath.Join(root, rel)
		body, ok, err := readIfPresent(path)
		if err != nil {
			return GalaxyManifest{}, err
		}
		if ok {
			return GalaxyManifest{Root: root, Path: path, Hash: hashGalaxy(body)}, nil
		}
	}
	return GalaxyManifest{Root: root}, nil
}

// hashGalaxy is the content hash the collections cache is keyed by. It
// deliberately does not mix a kind byte the way hashManifest does — there are
// no galaxy kinds today, so the extra byte could never vary; give it
// hashManifest's treatment if galaxy ever grows kinds.
func hashGalaxy(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:32]
}
