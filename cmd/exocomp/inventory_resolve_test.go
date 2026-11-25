package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

// TestResolveRunInventorySourceLinkedAndArtifact proves resolveRunInventory
// injects --inventory in-tree for source-linked static inventories and keeps
// materialized inventories on the isolated InventoryArtifact path.
func TestResolveRunInventorySourceLinkedAndArtifact(t *testing.T) {
	dir := initForSource(t)

	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "inventory"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	invPath := filepath.Join(checkout, "inventory", "hosts.yml")
	if err := os.WriteFile(invPath, []byte("all:\n  hosts:\n    nas:\n"), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	matPath := filepath.Join(t.TempDir(), "edge.json")
	if err := os.WriteFile(matPath, []byte(`{"all":{"hosts":{"e1":{}}}}`), 0o600); err != nil {
		t.Fatalf("write materialized: %v", err)
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
		Name: "proj-inv", SourceID: &src.ID, Path: "inventory/hosts.yml", Kind: store.InventoryStatic,
	}); err != nil {
		t.Fatalf("CreateInventory: %v", err)
	}
	if _, err := st.MaterializeInventory(ctx, "tf:edge", matPath); err != nil {
		t.Fatalf("MaterializeInventory: %v", err)
	}

	baseArgv := []string{"ansible-playbook", "--check", filepath.Join(checkout, "site.yml")}
	gotArgv, gotArt, err := resolveRunInventory(ctx, st, dir, baseArgv, "proj-inv")
	if err != nil {
		t.Fatalf("resolveRunInventory source-linked: %v", err)
	}
	if gotArt != "" {
		t.Errorf("artifact = %q, want empty for source-linked inventory", gotArt)
	}
	wantSourceArgv := []string{"ansible-playbook", "--inventory", invPath, "--check", filepath.Join(checkout, "site.yml")}
	if !slices.Equal(gotArgv, wantSourceArgv) {
		t.Fatalf("argv = %v, want %v", gotArgv, wantSourceArgv)
	}

	matArgv, matArt, err := resolveRunInventory(ctx, st, dir, baseArgv, "tf:edge")
	if err != nil {
		t.Fatalf("resolveRunInventory materialized: %v", err)
	}
	if matArt != matPath {
		t.Errorf("materialized artifact = %q, want %q", matArt, matPath)
	}
	if !slices.Equal(matArgv, baseArgv) {
		t.Errorf("materialized argv = %v, want unchanged %v", matArgv, baseArgv)
	}
}
