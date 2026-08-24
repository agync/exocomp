package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
)

// ResolveSourceHooks resolves a registered source's checkout root and its
// lifecycle (with the override-wins precedence). It is the transport-free read a
// CLI run and a server-side run share to decide whether a source's
// `.exocomp/hooks.yml` turns a run into a lifecycle workflow. A malformed hooks
// file is an error here, so a broken lifecycle fails before the run starts.
func (s *Service) ResolveSourceHooks(ctx context.Context, overridePath, sourceName string) (hooks.Hooks, string, error) {
	if err := ValidSourceName(sourceName); err != nil {
		return hooks.Hooks{}, "", err
	}
	src, err := s.store.SourceByName(ctx, sourceName)
	if errors.Is(err, store.ErrNotFound) {
		return hooks.Hooks{}, "", fmt.Errorf("no source %q", sourceName)
	}
	if err != nil {
		return hooks.Hooks{}, "", err
	}
	loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI)
	if err != nil {
		return hooks.Hooks{}, "", fmt.Errorf("source hooks: %w", err)
	}
	hk, err := hooks.Resolve(loc.Root, overridePath)
	if err != nil {
		return hooks.Hooks{}, "", err
	}
	return hk, loc.Root, nil
}

// ResolveSourcePlaybook reports whether a --source run is a hooked lifecycle
// and, when it is not, the playbook path rebased onto the source checkout so a
// plain play still runs from the source's own tree (loading its ./ansible.cfg).
// The hooked case is left to the caller's workflow composition, which rebases
// the path itself.
func (s *Service) ResolveSourcePlaybook(ctx context.Context, overridePath, sourceName, playbook string) (hooked bool, abs string, err error) {
	hk, root, err := s.ResolveSourceHooks(ctx, overridePath, sourceName)
	if err != nil {
		return false, "", err
	}
	if !hk.Empty() {
		return true, "", nil
	}
	// The rebase used to be a bare Join with no containment check, so
	// `../../../attacker.yml` resolved outside the checkout and an ABSOLUTE
	// playbook was not rebased at all: a run recorded against a signed source
	// then executed a tree the signature never covered. A
	// --source run reads from that source's checkout and nowhere else.
	if filepath.IsAbs(playbook) {
		return false, "", fmt.Errorf("source %q: playbook %q must be relative to the checkout: %w",
			sourceName, playbook, ErrSourcePathEscape)
	}
	confined, err := containedPath(root, playbook)
	if err != nil {
		return false, "", fmt.Errorf("source %q: %w", sourceName, err)
	}
	return false, confined, nil
}

// ErrSourcePathEscape reports a --source input that resolves outside the
// source's own checkout. A face renders it as an invalid argument: it is a bad
// input, not a server fault.
var ErrSourcePathEscape = errors.New("path resolves outside the source checkout")

// sourceNamePattern is the charset a source name may use. It is checked before
// the name is spliced into any path (a hooks override, a checkout root), so a
// traversal in the NAME cannot address a file outside the tree that holds it.
var sourceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ValidSourceName reports whether a source name is safe to splice into a path.
func ValidSourceName(name string) error {
	if !sourceNamePattern.MatchString(name) {
		return fmt.Errorf("source name %q must match [A-Za-z0-9_.-]+: %w", name, ErrSourcePathEscape)
	}
	return nil
}

// containedPath joins rel onto root and proves the result stays inside it, with
// symlinks resolved: a symlink inside the checkout pointing out of it is an
// escape too. The path itself need not exist yet; the deepest existing ancestor
// is what gets resolved.
func containedPath(root, rel string) (string, error) {
	realRoot, err := resolveExisting(root)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(realRoot, rel)
	target, err := resolveExisting(joined)
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(realRoot, target)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("playbook %q: %w", rel, ErrSourcePathEscape)
	}
	return joined, nil
}

// resolveExisting resolves symlinks on the deepest existing prefix of path and
// re-appends the rest, so a not-yet-created file still gets a real ancestor.
func resolveExisting(path string) (string, error) {
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("cannot resolve %q", path)
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}
