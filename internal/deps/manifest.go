package deps

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Manifest kinds. None means the project declares no Python dependencies, so a
// run stays on the image toolchain rather than getting its own venv.
const (
	KindNone         = "none"
	KindRequirements = "requirements"
	KindPyproject    = "pyproject"
)

// exocompDir is the per-source override directory; a manifest there wins over
// the repo-root default so an operator can pin exocomp's view of a project's
// dependencies without touching files the project's own tooling reads.
const exocompDir = ".exocomp"

// rootMarkers are the files/dirs that mark a project root when walking up from
// a playbook. .exocomp and a manifest are direct evidence; .git bounds the walk
// so discovery never climbs past the repository into a parent that happens to
// carry an unrelated manifest.
var rootMarkers = []string{exocompDir, "pyproject.toml", "requirements.txt", ".git"}

// PythonManifest is a source's resolved Python dependency manifest.
type PythonManifest struct {
	// Kind is one of the Kind* constants.
	Kind string
	// Root is the project root discovery settled on.
	Root string
	// Path is the absolute path of the manifest file, empty for KindNone.
	Path string
	// Hash is the content hash the venv cache is keyed by, empty for KindNone.
	Hash string
	// content is the raw manifest bytes, retained so venv creation reads the
	// exact bytes the hash was taken over.
	content []byte
}

// HasDeps reports whether the source declares a Python manifest at all. A
// source with none stays on the shared image toolchain (Decision 0011), so callers
// gate venv resolution on this.
func (m PythonManifest) HasDeps() bool { return m.Kind != KindNone && m.Kind != "" }

// FindProjectRoot walks up from start until it finds a directory carrying a
// root marker, returning that directory. It stops at the filesystem root and
// falls back to start when nothing is found, so a bare playbook in a directory
// with no manifest simply resolves to "no deps".
func FindProjectRoot(start string) string {
	dir := filepath.Clean(start)
	for {
		for _, marker := range rootMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Clean(start)
		}
		dir = parent
	}
}

// DiscoverPython resolves the Python manifest for the project rooted at (or
// above) start. Precedence follows Decision 0011: an .exocomp/requirements.txt
// override wins, then a repo-root pyproject.toml with PEP 621 dependencies,
// then a repo-root requirements.txt. A pyproject that declares no dependencies
// is treated as none, so a project carrying pyproject only for tool config does
// not get an empty venv.
func DiscoverPython(start string) (PythonManifest, error) {
	root := FindProjectRoot(start)

	// The explicit override is checked first and unconditionally: naming it is
	// how an operator overrides whatever the repo root carries.
	override := filepath.Join(root, exocompDir, "requirements.txt")
	if body, ok, err := readIfPresent(override); err != nil {
		return PythonManifest{}, err
	} else if ok {
		return requirementsManifest(root, override, body), nil
	}

	pyproject := filepath.Join(root, "pyproject.toml")
	if body, ok, err := readIfPresent(pyproject); err != nil {
		return PythonManifest{}, err
	} else if ok {
		specs, perr := parsePyprojectDeps(body)
		if perr != nil {
			return PythonManifest{}, fmt.Errorf("deps: %s: %w", pyproject, perr)
		}
		if len(specs) > 0 {
			return PythonManifest{
				Kind:    KindPyproject,
				Root:    root,
				Path:    pyproject,
				Hash:    hashManifest(KindPyproject, body),
				content: body,
			}, nil
		}
	}

	reqs := filepath.Join(root, "requirements.txt")
	if body, ok, err := readIfPresent(reqs); err != nil {
		return PythonManifest{}, err
	} else if ok {
		return requirementsManifest(root, reqs, body), nil
	}

	return PythonManifest{Kind: KindNone, Root: root}, nil
}

// requirementsManifest builds a requirements-kind manifest, treating a file
// with no dependency lines as none so an empty requirements.txt does not
// provision a venv that adds nothing.
func requirementsManifest(root, path string, body []byte) PythonManifest {
	if len(requirementLines(body)) == 0 {
		return PythonManifest{Kind: KindNone, Root: root}
	}
	return PythonManifest{
		Kind:    KindRequirements,
		Root:    root,
		Path:    path,
		Hash:    hashManifest(KindRequirements, body),
		content: body,
	}
}

// hashManifest is the content hash the venv cache is keyed by. The kind is
// mixed in so identical bytes under two manifest kinds never collide onto one
// venv, and the full file bytes are hashed: over-resolving on an unrelated edit
// is safe, under-resolving on a dependency change is not (Decision 0011 fail-safe).
func hashManifest(kind string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// readIfPresent reads path, reporting absence as (nil, false, nil) so a missing
// optional manifest is not an error while an unreadable one is.
func readIfPresent(path string) ([]byte, bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("deps: read %s: %w", path, err)
	}
	return body, true, nil
}
