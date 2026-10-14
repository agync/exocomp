package deps

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
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
// collections into a per-source cache dir for offline resolution (Decision 0015).
type GalaxyManifest struct {
	// Root is the project root discovery settled on, the namespace the
	// per-source collections cache is keyed by (PythonManifest.Root, mirrored).
	Root string
	// Path is the absolute requirements.yml, empty when none is present.
	Path string
	// Hash is a content hash over the file, empty when none is present.
	Hash string
	// LocalSources are the existing filesystem paths the requirements file
	// names as local collection artifacts (a `type: dir`/`type: file` entry, or
	// a bare path). ansible-galaxy reads them while resolving, and they usually
	// live outside Root — a source project referencing a sibling checkout — so
	// the resolver sandbox grants exactly these read-only on top of Root.
	LocalSources []string
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
			return GalaxyManifest{
				Root:         root,
				Path:         path,
				Hash:         hashGalaxy(body),
				LocalSources: galaxyLocalSources(body, root),
			}, nil
		}
	}
	return GalaxyManifest{Root: root}, nil
}

// galaxyLocalSources reports the filesystem paths a requirements file names as
// local collection artifacts: a mapping typed `dir` or `file` (whose path is
// `source` or `name`), or a bare scalar that resolves to an existing path. A
// relative path is resolved against the project root the way discovery keyed
// it, though ansible-galaxy itself resolves relative entries against its own
// working directory. A malformed or non-local file yields none — galaxy
// reports the parse failure itself, so discovery stays lenient.
func galaxyLocalSources(body []byte, root string) []string {
	var doc struct {
		Collections []any `yaml:"collections"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil
	}
	var out []string
	for _, entry := range doc.Collections {
		p := localCollectionEntry(entry, root)
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// localCollectionEntry is galaxyLocalSources' per-entry step: a scalar entry is
// the path itself, and a mapping only names a local artifact when its type is
// empty, `dir`, or `file` — a galaxy, git, or url entry is dialed, not read
// from disk.
func localCollectionEntry(entry any, root string) string {
	switch e := entry.(type) {
	case string:
		return localCollectionPath(e, root)
	case map[string]any:
		if t, _ := e["type"].(string); t != "" && t != "dir" && t != "file" {
			return ""
		}
		// `source` is the artifact pointer; `name` carries it for the local
		// types (ansible moves name into source once it sees a non-galaxy
		// type). Try both, since the file may use either shape.
		for _, key := range []string{"source", "name"} {
			if v, ok := e[key].(string); ok {
				if p := localCollectionPath(v, root); p != "" {
					return p
				}
			}
		}
	}
	return ""
}

// localCollectionPath resolves one candidate to an absolute existing path, or
// "" when it is not a filesystem artifact: an empty value, a galaxy-server or
// SCM pointer, or a path that does not exist. A path that does not exist is
// dropped because a sandbox grant attaches only to a real path, and galaxy will
// name the missing artifact far more clearly than a silent grant would.
func localCollectionPath(v, root string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.Contains(v, "://") ||
		strings.HasPrefix(v, "git+") || strings.HasPrefix(v, "git@") {
		return ""
	}
	if !filepath.IsAbs(v) {
		v = filepath.Join(root, v)
	}
	v = filepath.Clean(v)
	info, err := os.Stat(v)
	if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
		return ""
	}
	return v
}

// hashGalaxy is the content hash the collections cache is keyed by. It
// deliberately does not mix a kind byte the way hashManifest does — there are
// no galaxy kinds today, so the extra byte could never vary; give it
// hashManifest's treatment if galaxy ever grows kinds.
func hashGalaxy(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:32]
}
