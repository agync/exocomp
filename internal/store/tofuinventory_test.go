package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// TestStandingInventoryReadsWhatTheApplyLeft proves the standing inventory a
// schedule targets resolves to the bytes on disk and the time they were
// written, so a later fire reads whatever the last apply materialized.
func TestStandingInventoryReadsWhatTheApplyLeft(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "edge.json")
	body := []byte(`{"all": {"hosts": {"edge1": {}}}}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	if _, err := s.MaterializeInventory(ctx, "tf:edge", path); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	standing, err := s.StandingInventory(ctx, "tf:edge")
	if err != nil {
		t.Fatalf("standing: %v", err)
	}
	if string(standing.Body) != string(body) {
		t.Errorf("body = %q, want %q", standing.Body, body)
	}
	if standing.RefreshedAt.IsZero() {
		t.Error("RefreshedAt is zero; it is the freshness a schedule reads")
	}
}

// A registered inventory whose file is gone must fail rather than resolve to
// nothing: that is what stops a recurring play from running against an empty
// host list and reporting success.
func TestStandingInventoryFailsClosedWhenTheFileIsGone(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	if _, err := s.MaterializeInventory(ctx, "tf:edge", filepath.Join(t.TempDir(), "never-written.json")); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := s.StandingInventory(ctx, "tf:edge"); !errors.Is(err, store.ErrInventoryStale) {
		t.Fatalf("err = %v, want ErrInventoryStale", err)
	}
}

func TestStandingInventoryRejectsANonMaterializedRow(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	if _, err := s.CreateInventory(ctx, store.Inventory{
		Name: "cloud", Kind: store.InventoryDynamic, Path: "inv.yml",
	}); err != nil {
		t.Fatalf("create inventory: %v", err)
	}
	if _, err := s.StandingInventory(ctx, "cloud"); !errors.Is(err, store.ErrInventoryNotMaterialized) {
		t.Fatalf("err = %v, want ErrInventoryNotMaterialized", err)
	}
	if _, err := s.StandingInventory(ctx, "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
