package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// Errors a standing inventory resolves to when it cannot be run against. They
// are distinct from ErrNotFound — the name IS registered — because the caller's
// answer differs: an unregistered name is a typo, a stale one means the
// workspace has not applied since the file went away.
var (
	// ErrInventoryNotMaterialized reports a registered inventory that is not a
	// materialized file: a dynamic inventory, or a static row with no path.
	ErrInventoryNotMaterialized = errors.New("store: inventory is not a materialized file")
	// ErrInventoryStale reports a materialized inventory whose file is gone or
	// unreadable — the workspace has not applied since, or the artifact was
	// pruned. It is the fail-closed signal: never run a play against nothing.
	ErrInventoryStale = errors.New("store: materialized inventory is stale")
)

// MaterializeInventory records — or refreshes — the static inventory a
// workspace's tofu outputs were materialized into. It reuses the
// existing inventory table's `static` kind rather than a new table: the row is
// just a named path to the file an apply wrote, so a later `playbook run
// --inventory <name>` can target it and the operator can list it.
//
// It is an upsert on the unique name: every apply refreshes the same row's path
// rather than growing a row per apply. The path is all that changes; the file's
// contents are versioned by the artifact rows the apply job records.
func (s *Store) MaterializeInventory(ctx context.Context, name, path string) (Inventory, error) {
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO inventory (name, source_id, path, kind, env_binding_json, created_at)
		VALUES (?, NULL, ?, ?, '{}', ?)
		ON CONFLICT(name) DO UPDATE SET path = excluded.path`,
		name, path, InventoryStatic, now)
	if err != nil {
		return Inventory{}, wrap("materialize inventory", err)
	}
	return s.InventoryByName(ctx, name)
}

// StandingInventory is a materialized inventory resolved to what is behind it
// right now: the row, the bytes on disk, and when those bytes were last
// written. It is what makes `tf:<workspace>` a standing target a schedule or a
// reactive rule can name — the apply refreshes the file, and every later fire
// reads whatever the last apply left, new hosts included.
type StandingInventory struct {
	Inventory Inventory
	// Body is the materialized file's contents. A caller parses it to decide
	// whether there is anything to run against.
	Body []byte
	// RefreshedAt is the file's modification time: the moment the last
	// successful apply materialized it. There is no second timestamp column
	// because the file IS the state — a row claiming freshness the file does
	// not have would be the failure this type exists to prevent.
	RefreshedAt time.Time
}

// StandingInventory resolves a materialized inventory by name and reads it. It
// fails rather than returning an empty inventory: an unregistered name is
// ErrNotFound, a dynamic or pathless row is ErrInventoryNotMaterialized, and a
// file that is gone is ErrInventoryStale. Whether the bytes name any hosts is
// the caller's parse, but reaching that parse at all requires a real file.
func (s *Store) StandingInventory(ctx context.Context, name string) (StandingInventory, error) {
	inv, err := s.InventoryByName(ctx, name)
	if err != nil {
		return StandingInventory{}, err
	}
	if inv.Kind != InventoryStatic || inv.Path == "" {
		return StandingInventory{}, fmt.Errorf("%w: %s", ErrInventoryNotMaterialized, name)
	}
	info, err := os.Stat(inv.Path)
	if err != nil {
		return StandingInventory{}, fmt.Errorf("%w: %s: %w", ErrInventoryStale, name, err)
	}
	body, err := os.ReadFile(inv.Path)
	if err != nil {
		return StandingInventory{}, fmt.Errorf("%w: %s: %w", ErrInventoryStale, name, err)
	}
	return StandingInventory{
		Inventory:   inv,
		Body:        body,
		RefreshedAt: info.ModTime().UTC(),
	}, nil
}
