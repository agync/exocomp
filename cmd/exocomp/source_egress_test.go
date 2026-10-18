package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestSourceEgressDeclaresPerSource: declaring 443 on one source records it
// there and nowhere else — the CLI seam that feeds the queue's admission union.
func TestSourceEgressDeclaresPerSource(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateSource(ctx, store.Source{Name: "infra", Kind: store.SourceGit, Enabled: true}); err != nil {
		t.Fatalf("create infra: %v", err)
	}
	if _, err := st.CreateSource(ctx, store.Source{Name: "edge", Kind: store.SourceGit, Enabled: true}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	_ = st.Close()

	out, err := runCobra(t, newSourceCmd, "egress", "infra", "443")
	if err != nil {
		t.Fatalf("source egress: %v (%s)", err, out)
	}
	if !strings.Contains(out, "443") {
		t.Fatalf("egress output = %q, want the granted port", out)
	}

	st2, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st2.Close() }()

	infra, err := st2.SourceByName(ctx, "infra")
	if err != nil {
		t.Fatalf("by name infra: %v", err)
	}
	if len(infra.ExtraEgressPorts) != 1 || infra.ExtraEgressPorts[0] != 443 {
		t.Fatalf("infra egress = %v, want [443]", infra.ExtraEgressPorts)
	}
	edge, err := st2.SourceByName(ctx, "edge")
	if err != nil {
		t.Fatalf("by name edge: %v", err)
	}
	if len(edge.ExtraEgressPorts) != 0 {
		t.Fatalf("sibling edge egress = %v, want empty", edge.ExtraEgressPorts)
	}
}

// TestSourceEgressRefusesJunk: an out-of-range port is refused at the CLI, so a
// junk declaration never reaches a run's profile.
func TestSourceEgressRefusesJunk(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateSource(ctx, store.Source{Name: "infra", Kind: store.SourceGit, Enabled: true}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = st.Close()

	if _, err := runCobra(t, newSourceCmd, "egress", "infra", "70000"); err == nil {
		t.Fatal("egress 70000 was accepted, want an out-of-range refusal")
	}
	if _, err := runCobra(t, newSourceCmd, "egress", "infra", "not-a-port"); err == nil {
		t.Fatal("egress not-a-port was accepted, want a parse refusal")
	}
}
