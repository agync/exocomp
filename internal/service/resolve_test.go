package service_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// TestResolveInventoryByNameSourceRelative: a registered inventory whose path is
// relative and whose source is set resolves against the source's checkout root,
// not the process CWD — the same rule the CLI's --inventory <name> relies on,
// now proven at the transport-free seam the API also calls.
func TestResolveInventoryByNameSourceRelative(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	dataDir := t.TempDir()

	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "inventory.ini"), []byte("[all]\nnode0\n"), 0o644); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	src, err := st.CreateSource(ctx, store.Source{Name: "proj", Kind: store.SourcePath, URI: checkout, Enabled: true})
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if _, err := st.CreateInventory(ctx, store.Inventory{
		Name: "inv", SourceID: &src.ID, Path: "inventory.ini", Kind: store.InventoryStatic,
	}); err != nil {
		t.Fatalf("CreateInventory: %v", err)
	}

	// Stand somewhere unrelated: a CWD-relative resolution would land here.
	elsewhere := t.TempDir()
	prev, _ := os.Getwd()
	if err := os.Chdir(elsewhere); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(prev) }()

	path, name, err := service.New(st, dataDir, nil).ResolveInventory(ctx, "inv")
	if err != nil {
		t.Fatalf("ResolveInventory: %v", err)
	}
	if name != "inv" {
		t.Errorf("name = %q, want inv", name)
	}
	if want := filepath.Join(checkout, "inventory.ini"); path != want {
		t.Fatalf("path = %q, want %q (must resolve against the source checkout)", path, want)
	}
}

// TestResolveInventoryPathValueIsAbsolute: a value that matches no registered
// inventory is an absolute filesystem path with no name attached.
func TestResolveInventoryPathValueIsAbsolute(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)

	path, name, err := service.New(st, t.TempDir(), nil).ResolveInventory(ctx, "hosts.ini")
	if err != nil {
		t.Fatalf("ResolveInventory: %v", err)
	}
	if name != "" {
		t.Errorf("name = %q, want empty for a bare path", name)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("path = %q, want absolute", path)
	}
}

// TestResolveInventoryEmptyResolvesToNothing: an empty value resolves to neither
// a path nor a name, so the argv builder adds no --inventory and ansible defers
// to its own inventory discovery.
func TestResolveInventoryEmptyResolvesToNothing(t *testing.T) {
	path, name, err := service.New(openStore(t), t.TempDir(), nil).ResolveInventory(context.Background(), "")
	if err != nil {
		t.Fatalf("ResolveInventory: %v", err)
	}
	if path != "" || name != "" {
		t.Errorf("empty inventory = %q/%q, want empty/empty", path, name)
	}
}

// TestResolveSourcePlaybookRebasesPath: a source with no hooks rebases a
// relative playbook onto its checkout root, so a plain play runs from the
// source's own tree; hooked is false.
func TestResolveSourcePlaybookRebasesPath(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	dataDir := t.TempDir()

	checkout := t.TempDir()
	if _, err := st.CreateSource(ctx, store.Source{Name: "proj", Kind: store.SourcePath, URI: checkout, Enabled: true}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	hooked, abs, err := service.New(st, dataDir, nil).ResolveSourcePlaybook(ctx,
		filepath.Join(dataDir, "overrides", "proj.yml"), "proj", "site.yml")
	if err != nil {
		t.Fatalf("ResolveSourcePlaybook: %v", err)
	}
	if hooked {
		t.Error("a source with no hooks must not be hooked")
	}
	if want := filepath.Join(checkout, "site.yml"); abs != want {
		t.Fatalf("playbook = %q, want %q", abs, want)
	}
}

// TestResolveSourcePlaybookUnknownSource: an unknown source is a clear error, so
// a bad --source fails before the run starts.
func TestResolveSourcePlaybookUnknownSource(t *testing.T) {
	_, _, err := service.New(openStore(t), t.TempDir(), nil).ResolveSourcePlaybook(
		context.Background(), "", "ghost", "site.yml")
	if err == nil {
		t.Fatal("ResolveSourcePlaybook on an unknown source returned no error")
	}
}

// TestResolveSourcePlaybookConfined proves a rebase cannot escape the confined playbook path: the rebase was a
// bare filepath.Join with no containment check, so `../../../../tmp/attacker.yml`
// resolved outside the checkout and an absolute path was not rebased at all —
// a run recorded against a signed source executed a tree the signature never
// covered. Both escapes are refused now.
func TestResolveSourcePlaybookConfined(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	dataDir := t.TempDir()

	checkout := t.TempDir()
	outside := filepath.Join(t.TempDir(), "attacker.yml")
	if err := os.WriteFile(outside, []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("write attacker playbook: %v", err)
	}
	if _, err := st.CreateSource(ctx, store.Source{
		Name: "signed-proj", Kind: store.SourcePath, URI: checkout, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	svc := service.New(st, dataDir, nil)

	for _, playbook := range []string{
		"../../../../../../tmp/attacker.yml",
		outside,
		"nested/../../escape.yml",
	} {
		_, abs, err := svc.ResolveSourcePlaybook(ctx, "", "signed-proj", playbook)
		if err == nil {
			t.Errorf("playbook %q resolved to %q, want a refusal outside the checkout", playbook, abs)
		}
	}

	// A path inside the checkout still resolves, including through a subdirectory.
	if err := os.MkdirAll(filepath.Join(checkout, "plays"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "plays", "site.yml"), []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	_, abs, err := svc.ResolveSourcePlaybook(ctx, "", "signed-proj", "plays/site.yml")
	if err != nil {
		t.Fatalf("in-checkout playbook: %v", err)
	}
	if want := filepath.Join(checkout, "plays", "site.yml"); abs != want {
		t.Fatalf("playbook = %q, want %q", abs, want)
	}
}

// TestResolveSourceNameCharset: a source name is validated before it is spliced
// into any path, so a traversal in the NAME cannot reach a hooks override file
// outside the data dir.
func TestResolveSourceNameCharset(t *testing.T) {
	svc := service.New(openStore(t), t.TempDir(), nil)
	for _, name := range []string{"../../etc", "proj/../../x", "a b", "pro:j"} {
		if _, _, err := svc.ResolveSourcePlaybook(context.Background(), "", name, "site.yml"); err == nil {
			t.Errorf("source name %q was accepted", name)
		}
	}
}
