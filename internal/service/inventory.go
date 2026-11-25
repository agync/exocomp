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

// ResolveStandingInventory resolves a registered static standing inventory by
// name, reads its bytes from disk, and reports whether it lives inside a
// registered source checkout rather than being a tofu-materialized artifact.
func (s *Service) ResolveStandingInventory(ctx context.Context, name string) (store.StandingInventory, string, bool, error) {
	inv, err := s.store.InventoryByName(ctx, name)
	if err != nil {
		return store.StandingInventory{}, "", false, err
	}
	if inv.Kind != store.InventoryStatic || inv.Path == "" {
		return store.StandingInventory{}, "", false, fmt.Errorf("%w: %s", store.ErrInventoryNotMaterialized, name)
	}
	resolved, err := s.resolveInventoryPath(ctx, inv)
	if err != nil {
		return store.StandingInventory{}, "", false, err
	}
	standing, err := store.StandingInventoryAt(inv, resolved)
	if err != nil {
		return store.StandingInventory{}, "", false, err
	}
	return standing, resolved, inv.SourceID != nil, nil
}

// resolveInventoryPath turns a registered inventory's stored path into the path
// ansible targets. A source-linked inventory records its path relative to that
// source's checkout, so it must be relative and resolve inside the source's
// materialized root — the same directory the play runs from — not the process
// CWD. An inventory with no source keeps its stored path as-is.
func (s *Service) resolveInventoryPath(ctx context.Context, inv store.Inventory) (string, error) {
	if inv.Path == "" || inv.SourceID == nil {
		return inv.Path, nil
	}
	if filepath.IsAbs(inv.Path) {
		return "", fmt.Errorf("inventory %q: path %q must be relative to the checkout: %w",
			inv.Name, inv.Path, ErrSourcePathEscape)
	}
	src, err := s.store.GetSource(ctx, *inv.SourceID)
	if err != nil {
		return "", fmt.Errorf("inventory %q: source: %w", inv.Name, err)
	}
	loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return "", fmt.Errorf("inventory %q: %w", inv.Name, err)
	}
	confined, err := containedPathKind(loc.Root, "inventory", inv.Path)
	if err != nil {
		return "", fmt.Errorf("inventory %q: %w", inv.Name, err)
	}
	return confined, nil
}
