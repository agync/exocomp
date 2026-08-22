package store_test

import (
	"context"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestMaterializeInventoryUpserts proves a workspace's materialized inventory
// is one row refreshed per apply, not a new row each time, and that it is a
// static inventory pointing at the file the apply wrote.
func TestMaterializeInventoryUpserts(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	first, err := s.MaterializeInventory(ctx, "tf:edge", "/data/artifacts/inventory/edge.json")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if first.Kind != store.InventoryStatic {
		t.Errorf("kind = %q, want %q", first.Kind, store.InventoryStatic)
	}
	if first.SourceID != nil {
		t.Errorf("materialized inventory should have no source: %+v", first.SourceID)
	}

	// A second apply refreshes the same named row (path may change if the
	// convention ever moves) rather than colliding on the unique name.
	second, err := s.MaterializeInventory(ctx, "tf:edge", "/data/artifacts/inventory/edge-v2.json")
	if err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("upsert created a new row: %d != %d", second.ID, first.ID)
	}
	if second.Path != "/data/artifacts/inventory/edge-v2.json" {
		t.Errorf("path not refreshed: %q", second.Path)
	}

	all, err := s.ListInventories(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("materialize made %d rows, want 1", len(all))
	}

	// The row resolves by name, which is how a play targets it.
	byName, err := s.InventoryByName(ctx, "tf:edge")
	if err != nil {
		t.Fatalf("by name: %v", err)
	}
	if byName.Path != second.Path {
		t.Errorf("byName path = %q, want %q", byName.Path, second.Path)
	}
}
