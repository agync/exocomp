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
// input to `playbook vendor` (cmd/exocomp/vendor.go), which installs the
// collections into cache/collections for offline resolution.
type GalaxyManifest struct {
	// Path is the absolute requirements.yml, empty when none is present.
	Path string
	// Hash is a content hash over the file, empty when none is present.
	Hash string
}

// Found reports whether a collections requirements file was discovered.
func (g GalaxyManifest) Found() bool { return g.Path != "" }

// DiscoverGalaxy resolves the collections requirements file for the project
// rooted at (or above) start: an .exocomp/requirements.yml override, else a
// repo-root requirements.yml. It only locates the file — the existing collections
// vendor performs the install — so discovery and vendoring stay one seam.
func DiscoverGalaxy(start string) (GalaxyManifest, error) {
	root := FindProjectRoot(start)
	for _, rel := range []string{galaxyOverride, galaxyRoot} {
		path := filepath.Join(root, rel)
		body, ok, err := readIfPresent(path)
		if err != nil {
			return GalaxyManifest{}, err
		}
		if ok {
			return GalaxyManifest{Path: path, Hash: hashGalaxy(body)}, nil
		}
	}
	return GalaxyManifest{}, nil
}

func hashGalaxy(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:32]
}
