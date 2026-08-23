package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestWorkspaceEgressDeclaresPerWorkspace: declaring 6443 on one workspace
// records it there and nowhere else — the CLI seam that feeds TofuOptions.
func TestWorkspaceEgressDeclaresPerWorkspace(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: "k8s", TofuDir: "/srv/k8s"}); err != nil {
		t.Fatalf("create k8s: %v", err)
	}
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: "edge", TofuDir: "/srv/edge"}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	_ = st.Close()

	out, err := runCobra(t, newWorkspaceCmd, "egress", "k8s", "6443")
	if err != nil {
		t.Fatalf("workspace egress: %v (%s)", err, out)
	}
	if !strings.Contains(out, "6443") {
		t.Fatalf("egress output = %q, want the granted port", out)
	}

	st2, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st2.Close() }()

	k8s, err := st2.WorkspaceByName(ctx, "k8s")
	if err != nil {
		t.Fatalf("by name k8s: %v", err)
	}
	if len(k8s.ExtraEgressPorts) != 1 || k8s.ExtraEgressPorts[0] != 6443 {
		t.Fatalf("k8s egress = %v, want [6443]", k8s.ExtraEgressPorts)
	}
	edge, err := st2.WorkspaceByName(ctx, "edge")
	if err != nil {
		t.Fatalf("by name edge: %v", err)
	}
	if len(edge.ExtraEgressPorts) != 0 {
		t.Fatalf("sibling edge egress = %v, want empty", edge.ExtraEgressPorts)
	}
}

// TestWorkspaceEgressRefusesJunk: an out-of-range port is refused at the CLI,
// so a junk declaration never reaches a run's profile.
func TestWorkspaceEgressRefusesJunk(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: "k8s", TofuDir: "/srv/k8s"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = st.Close()

	if _, err := runCobra(t, newWorkspaceCmd, "egress", "k8s", "70000"); err == nil {
		t.Fatal("egress 70000 was accepted, want an out-of-range refusal")
	}
	if _, err := runCobra(t, newWorkspaceCmd, "egress", "k8s", "not-a-port"); err == nil {
		t.Fatal("egress not-a-port was accepted, want a parse refusal")
	}
}
