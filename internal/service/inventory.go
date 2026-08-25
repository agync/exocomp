package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
)

// ResolveInventory turns an --inventory value into the path ansible targets and,
// when it names a registered inventory, that inventory's name so its env_binding
// resolves for a dynamic plugin. A value that matches no registered inventory is
// treated as a filesystem path; an empty value resolves to nothing, so the argv
// carries no --inventory and ansible discovers its own. This is the
// transport-free half a CLI run and a server-side run both call, so the two turn
// an operator's names into the same paths.
func (s *Service) ResolveInventory(ctx context.Context, value string) (path, name string, err error) {
	if value == "" {
		return "", "", nil
	}
	if inv, ierr := s.store.InventoryByName(ctx, value); ierr == nil {
		resolved, rerr := s.resolveInventoryPath(ctx, inv)
		if rerr != nil {
			return "", "", rerr
		}
		return resolved, inv.Name, nil
	} else if !errors.Is(ierr, store.ErrNotFound) {
		return "", "", ierr
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", "", fmt.Errorf("resolve inventory: %w", err)
	}
	return abs, "", nil
}

// resolveInventoryPath turns a registered inventory's stored path into the path
// ansible targets. A source-linked inventory records its path relative to that
// source's checkout, so it must resolve against the source's materialized root —
// the same directory the play runs from — not the process CWD. An absolute path,
// or an inventory with no source, is returned as stored.
func (s *Service) resolveInventoryPath(ctx context.Context, inv store.Inventory) (string, error) {
	if inv.Path == "" || filepath.IsAbs(inv.Path) || inv.SourceID == nil {
		return inv.Path, nil
	}
	src, err := s.store.GetSource(ctx, *inv.SourceID)
	if err != nil {
		return "", fmt.Errorf("inventory %q: source: %w", inv.Name, err)
	}
	loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return "", fmt.Errorf("inventory %q: %w", inv.Name, err)
	}
	return filepath.Join(loc.Root, inv.Path), nil
}
