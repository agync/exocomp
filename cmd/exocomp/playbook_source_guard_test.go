package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestSourcePlaybookRefusesEscape proves sourcePlaybook confines a --source
// run's playbook to the source checkout: a `../` traversal and an absolute
// path both used to pass a bare filepath.Join straight through, escaping the
// tree a signed/registered source is supposed to confine the run to.
func TestSourcePlaybookRefusesEscape(t *testing.T) {
	initForSource(t)

	st, err := openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	checkout := t.TempDir()
	if _, err := st.CreateSource(ctx, store.Source{
		Name: "proj", Kind: store.SourcePath, URI: checkout, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "attacker.yml")
	if err := os.WriteFile(outside, []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("write attacker playbook: %v", err)
	}

	for _, playbook := range []string{
		"../../../etc/x.yml",
		outside,
	} {
		hooked, abs, err := sourcePlaybook(ctx, io.Discard, "proj", playbook)
		if err == nil {
			t.Errorf("playbook %q resolved to hooked=%v abs=%q, want a refusal outside the checkout",
				playbook, hooked, abs)
		}
	}
}

// TestSourcePlaybookResolvesRelative proves the legitimate case is unaffected
// by the guard: a plain relative playbook inside the source still resolves to
// <checkout>/<playbook>, hooked false.
func TestSourcePlaybookResolvesRelative(t *testing.T) {
	initForSource(t)

	st, err := openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	checkout := t.TempDir()
	if _, err := st.CreateSource(ctx, store.Source{
		Name: "proj", Kind: store.SourcePath, URI: checkout, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "site.yml"), []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	hooked, abs, err := sourcePlaybook(ctx, io.Discard, "proj", "site.yml")
	if err != nil {
		t.Fatalf("sourcePlaybook: %v", err)
	}
	if hooked {
		t.Error("a source with no hooks must not be hooked")
	}
	if want := filepath.Join(checkout, "site.yml"); abs != want {
		t.Fatalf("playbook = %q, want %q", abs, want)
	}
}
