// Package source resolves where an Ansible or OpenTofu run's tree comes from
// and keeps a git source synced to a pinned ref (Decision 0010). A source is one of
// two localities — a registered git repo, or a path already on the box
// (absolute, or relative to the data volume) — and the credential planes are
// identical across both; only how the tree arrives differs.
//
// The package shells out to the pinned git binary and never reads the store or
// the secrets envelope: the caller resolves the deploy key from the envelope
// and hands the bytes in, and this package writes them to a 0700 file only for
// the length of one git command.
package source

import (
	"fmt"
	"path/filepath"
	"strings"

	"exocomp.dev/exocomp/internal/naming"
)

// Locality kinds mirror store's source kinds so the CLI can pass a source row's
// kind straight through.
const (
	KindGit  = "git"
	KindPath = "path"
)

// checkoutDir is the subdirectory of the data dir git sources are cloned into.
const checkoutDir = "sources"

// Locality is a resolved source tree: where a run finds it and whether the run
// may write there.
type Locality struct {
	Kind string
	Name string
	// URI is the clone URL for a git source, or the tree path for a path
	// locality.
	URI string
	// Root is the checkout root a run uses as its working directory (spec.Dir),
	// so the repo's own ./ansible.cfg loads (the expert-review must-fix).
	Root string
	// ReadOnly is true when the sandbox must grant the tree read access only.
	// A path source is read-only unless registered --writable; a git checkout
	// is always writable.
	ReadOnly bool
}

// Resolve turns a source's kind, name, uri, and writable bit into a Locality.
// dataDir is the 0700 data volume; a git source is cloned under it, and a path
// source is either an absolute tree or a path relative to the data volume.
//
// A git source's checkout root is derived from the source name, never from the
// URI, so a crafted URI cannot escape the sources directory. A path source is
// read-only unless writable; an absolute path is cleaned as-is (the sandbox
// guard still refuses an ancestor of the data dir), and a relative path is
// rejected if it is empty, ".", or escapes with "..", then joined under dataDir
// and asserted to stay strictly beneath it.
func Resolve(dataDir, kind, name, uri string, writable bool) (Locality, error) {
	if strings.TrimSpace(name) == "" {
		return Locality{}, fmt.Errorf("source: empty name")
	}
	loc := Locality{Kind: kind, Name: name, URI: uri}

	switch kind {
	case KindGit:
		if strings.TrimSpace(uri) == "" {
			return Locality{}, fmt.Errorf("source %q: git source needs a url", name)
		}
		if dataDir == "" {
			return Locality{}, fmt.Errorf("source %q: no data dir for the checkout", name)
		}
		if !naming.Component(name) {
			return Locality{}, fmt.Errorf("source %q: name is not a safe directory component", name)
		}
		loc.Root = filepath.Join(dataDir, checkoutDir, name)
	case KindPath:
		loc.ReadOnly = !writable
		if filepath.IsAbs(uri) {
			loc.Root = filepath.Clean(uri)
			break
		}
		root, err := resolveRelative(dataDir, name, uri)
		if err != nil {
			return Locality{}, err
		}
		loc.Root = root
	default:
		return Locality{}, fmt.Errorf("source %q: unknown kind %q", name, kind)
	}
	return loc, nil
}

// resolveRelative joins a relative path source under the data volume. It rejects
// an empty or "." path (which would name the data dir itself) and any ".."
// component (which would climb out), then asserts the cleaned join stays
// strictly under dataDir — a structural guarantee that a relative source can
// never be, nor escape, the data dir.
func resolveRelative(dataDir, name, uri string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("source %q: no data dir for a relative path", name)
	}
	clean := filepath.Clean(uri)
	if clean == "." || clean == "" {
		return "", fmt.Errorf("source %q: relative path is empty", name)
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("source %q: relative path %q escapes the data volume", name, uri)
		}
	}
	root := filepath.Join(dataDir, clean)
	base := filepath.Clean(dataDir)
	if root == base || !strings.HasPrefix(root, base+string(filepath.Separator)) {
		return "", fmt.Errorf("source %q: relative path %q does not stay under the data volume", name, uri)
	}
	return root, nil
}
