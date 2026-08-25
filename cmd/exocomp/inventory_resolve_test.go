package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestResolveInventoryArgSourceRelative proves BUG 1's fix: a registered
// inventory whose path is relative and whose source is set resolves against the
// source's checkout root, not the process CWD. The test chdirs somewhere
// unrelated first, so a CWD-relative resolution (the bug) would produce a
// different, wrong path than the source-root join this asserts.
func TestResolveInventoryArgSourceRelative(t *testing.T) {
	dir := initForSource(t)

	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "inventory.ini"), []byte("[all]\nnode0\n"), 0o644); err != nil {
		t.Fatalf("write inventory fixture: %v", err)
	}

	st, err := openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	src, err := st.CreateSource(ctx, store.Source{Name: "proj", Kind: store.SourcePath, URI: checkout, Enabled: true})
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if _, err := st.CreateInventory(ctx, store.Inventory{
		Name:     "inv",
		SourceID: &src.ID,
		Path:     "inventory.ini",
		Kind:     store.InventoryStatic,
	}); err != nil {
		t.Fatalf("CreateInventory: %v", err)
	}

	// Stand somewhere with no relation to the checkout: were the path resolved
	// against the CWD, it would land here and fail the assertion below.
	elsewhere := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(elsewhere); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(prev) }()

	path, name, err := resolveInventoryArg(ctx, st, dir, "inv")
	if err != nil {
		t.Fatalf("resolveInventoryArg: %v", err)
	}
	if name != "inv" {
		t.Errorf("name = %q, want inv", name)
	}
	want := filepath.Join(checkout, "inventory.ini")
	if path != want {
		t.Fatalf("path = %q, want %q (must resolve against the source checkout, not the CWD)", path, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("resolved inventory does not exist on disk: %v", err)
	}
}

// TestResolveInventoryArgAbsolutePathUnchanged proves an inventory with no
// source, or an absolute path, is returned as stored: the source-relative rule
// applies only to a source-linked relative path.
func TestResolveInventoryArgAbsolutePathUnchanged(t *testing.T) {
	dir := initForSource(t)

	st, err := openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	abs := filepath.Join(t.TempDir(), "hosts.ini")
	if _, err := st.CreateInventory(ctx, store.Inventory{
		Name: "abs",
		Path: abs,
		Kind: store.InventoryStatic,
	}); err != nil {
		t.Fatalf("CreateInventory: %v", err)
	}

	path, _, err := resolveInventoryArg(ctx, st, dir, "abs")
	if err != nil {
		t.Fatalf("resolveInventoryArg: %v", err)
	}
	if path != abs {
		t.Fatalf("path = %q, want the stored absolute %q", path, abs)
	}
}
