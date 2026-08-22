package store

import (
	"context"
	"time"
)

// MaterializeInventory records — or refreshes — the static inventory a
// workspace's tofu outputs were materialized into (story 5). It reuses the
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
