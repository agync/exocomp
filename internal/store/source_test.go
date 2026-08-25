package store_test

import (
	"context"
	"errors"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

func TestSourceCRUD(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	created, err := s.CreateSource(ctx, store.Source{
		Name:            "infra",
		Kind:            store.SourceGit,
		URI:             "git@forge:org/infra.git",
		Ref:             "main",
		DeployKeySecret: "source/infra/deploy-key",
		Signed:          true,
		AllowedSigners:  "* ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAtrusted\n",
		Enabled:         true,
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if created.ID == 0 || !created.Signed || !created.Enabled {
		t.Fatalf("created = %+v", created)
	}

	byName, err := s.SourceByName(ctx, "infra")
	if err != nil {
		t.Fatalf("source by name: %v", err)
	}
	if byName.URI != "git@forge:org/infra.git" || byName.Ref != "main" {
		t.Fatalf("byName = %+v", byName)
	}
	if byName.AllowedSigners != "* ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAtrusted\n" {
		t.Fatalf("allowed signers did not round-trip: %q", byName.AllowedSigners)
	}

	if _, err := s.CreateSource(ctx, store.Source{Name: "infra", Kind: store.SourceGit}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate name: want ErrDuplicate, got %v", err)
	}

	if err := s.RecordSourceSync(ctx, created.ID, "v1.4.0", "deadbeef"); err != nil {
		t.Fatalf("record sync: %v", err)
	}
	synced, _ := s.GetSource(ctx, created.ID)
	if synced.Ref != "v1.4.0" || synced.LastSyncSHA != "deadbeef" {
		t.Fatalf("after sync = %+v", synced)
	}

	if err := s.SetSourceEnabled(ctx, created.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got, _ := s.GetSource(ctx, created.ID); got.Enabled {
		t.Fatal("source still enabled after disable")
	}

	if err := s.RecordSourceSync(ctx, 9999, "x", "y"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sync missing source: want ErrNotFound, got %v", err)
	}

	if err := s.DeleteSource(ctx, "infra"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteSource(ctx, "infra"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete twice: want ErrNotFound, got %v", err)
	}
}

func TestSourceKindConstraint(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.CreateSource(ctx, store.Source{
		Name: "bad", Kind: "svn", Enabled: true,
	}); err == nil {
		t.Fatal("want CHECK constraint rejection for kind svn, got nil")
	}
	// The collapsed model no longer admits the old kind names.
	for _, kind := range []string{"hostpath", "volpath"} {
		if _, err := s.CreateSource(ctx, store.Source{Name: kind, Kind: kind, Enabled: true}); err == nil {
			t.Fatalf("want CHECK rejection for retired kind %q, got nil", kind)
		}
	}
}

func TestSourcePathWritableRoundTrips(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	ro, err := s.CreateSource(ctx, store.Source{
		Name: "ro", Kind: store.SourcePath, URI: "/srv/tree", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create read-only path source: %v", err)
	}
	if ro.Kind != store.SourcePath || ro.Writable {
		t.Fatalf("read-only path source = %+v, want kind path writable false", ro)
	}

	rw, err := s.CreateSource(ctx, store.Source{
		Name: "rw", Kind: store.SourcePath, URI: "checkouts/app", Writable: true, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create writable path source: %v", err)
	}
	got, err := s.SourceByName(ctx, "rw")
	if err != nil {
		t.Fatalf("source by name: %v", err)
	}
	if !got.Writable || got.URI != "checkouts/app" {
		t.Fatalf("writable did not round-trip: %+v (created %+v)", got, rw)
	}
}

func TestInventoryCRUDAndCascade(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	src, err := s.CreateSource(ctx, store.Source{Name: "cloud", Kind: store.SourceGit, Enabled: true})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	inv, err := s.CreateInventory(ctx, store.Inventory{
		Name:     "proxmox",
		SourceID: &src.ID,
		Path:     "inventory/proxmox.yml",
		Kind:     store.InventoryDynamic,
		EnvBinding: map[string]string{
			"PROXMOX_PASSWORD": "inventory/proxmox/password",
			"PROXMOX_USER":     "inventory/proxmox/user",
		},
	})
	if err != nil {
		t.Fatalf("create inventory: %v", err)
	}
	if inv.ID == 0 {
		t.Fatal("inventory id is 0")
	}

	got, err := s.InventoryByName(ctx, "proxmox")
	if err != nil {
		t.Fatalf("inventory by name: %v", err)
	}
	if got.EnvBinding["PROXMOX_PASSWORD"] != "inventory/proxmox/password" {
		t.Fatalf("env binding round-trip lost: %+v", got.EnvBinding)
	}
	if got.SourceID == nil || *got.SourceID != src.ID {
		t.Fatalf("source id = %v", got.SourceID)
	}

	list, err := s.ListInventories(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list inventories = %d, %v", len(list), err)
	}

	// Deleting the source cascades the inventory (foreign key ON DELETE CASCADE).
	if err := s.DeleteSource(ctx, "cloud"); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	if _, err := s.InventoryByName(ctx, "proxmox"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("inventory should have cascaded: %v", err)
	}
}

func TestRunSecretBinding(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	for _, rs := range []store.RunSecret{
		{Binding: "proxmox", EnvName: "PROXMOX_PASSWORD", SecretName: "provider/proxmox/password"},
		{Binding: "proxmox", EnvName: "PROXMOX_USER", SecretName: "provider/proxmox/user"},
		{Binding: "cloudflare", EnvName: "CLOUDFLARE_API_TOKEN", SecretName: "provider/cf/token"},
	} {
		if _, err := s.AddRunSecret(ctx, rs); err != nil {
			t.Fatalf("add run secret %+v: %v", rs, err)
		}
	}

	// The same env name twice in one binding is an operator error.
	if _, err := s.AddRunSecret(ctx, store.RunSecret{
		Binding: "proxmox", EnvName: "PROXMOX_PASSWORD", SecretName: "other",
	}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate env in binding: want ErrDuplicate, got %v", err)
	}

	pairs, err := s.RunSecretsByBinding(ctx, "proxmox")
	if err != nil {
		t.Fatalf("by binding: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("proxmox binding has %d pairs, want 2", len(pairs))
	}
	if pairs[0].EnvName != "PROXMOX_PASSWORD" || pairs[1].EnvName != "PROXMOX_USER" {
		t.Fatalf("pairs not ordered by env name: %+v", pairs)
	}

	bindings, err := s.ListRunSecretBindings(ctx)
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 2 || bindings[0] != "cloudflare" || bindings[1] != "proxmox" {
		t.Fatalf("bindings = %v", bindings)
	}

	if err := s.DeleteRunSecretBinding(ctx, "proxmox"); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	if got, _ := s.RunSecretsByBinding(ctx, "proxmox"); len(got) != 0 {
		t.Fatalf("binding not deleted: %+v", got)
	}
	if err := s.DeleteRunSecretBinding(ctx, "proxmox"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete missing binding: want ErrNotFound, got %v", err)
	}
}
