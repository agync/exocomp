// Package source resolves where an Ansible or OpenTofu run's tree comes from
// and keeps a git source synced to a pinned ref (ADR 0010). A source is one of
// three localities — a registered git repo, a host path bind-mounted into the
// container, or an in-container path in the data volume — and the credential
// planes are identical across all three; only how the tree arrives differs.
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
	KindGit      = "git"
	KindHostPath = "hostpath"
	KindVolPath  = "volpath"
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
	// ReadOnly is true for a host path: the operator owns the tree and the
	// sandbox bind-mounts it read-only by default.
	ReadOnly bool
}

// Resolve turns a source's kind, name, and uri into a Locality. dataDir is the
// 0700 data volume; a git source is cloned under it, a volpath is expected to
// already live there, and a hostpath is wherever the operator mounted it.
//
// A git source's checkout root is derived from the source name, never from the
// URI, so a crafted URI cannot escape the sources directory. Path localities
// are cleaned and required to be absolute: a relative source path would mean
// "wherever the daemon happens to run", which the sandbox guard also refuses.
func Resolve(dataDir, kind, name, uri string) (Locality, error) {
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
	case KindHostPath:
		loc.ReadOnly = true
		fallthrough
	case KindVolPath:
		if !filepath.IsAbs(uri) {
			return Locality{}, fmt.Errorf("source %q: %s path %q must be absolute", name, kind, uri)
		}
		loc.Root = filepath.Clean(uri)
	default:
		return Locality{}, fmt.Errorf("source %q: unknown kind %q", name, kind)
	}
	return loc, nil
}
