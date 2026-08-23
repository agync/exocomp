package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

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
	if !filepath.IsAbs(playbook) {
		playbook = filepath.Join(root, playbook)
	}
	return false, playbook, nil
}
