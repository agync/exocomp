package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestWorkspaceOfflineProvidersIsPerWorkspace: flipping offline on one
// workspace records it there and nowhere else, and the store reads it back —
// the CLI seam that feeds TofuOptions.OfflineProviders.
func TestWorkspaceOfflineProvidersIsPerWorkspace(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: "cold", TofuDir: "/srv/cold"}); err != nil {
		t.Fatalf("create cold: %v", err)
	}
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: "edge", TofuDir: "/srv/edge"}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	_ = st.Close()

	out, err := runCobra(t, newWorkspaceCmd, "offline", "cold", "on")
	if err != nil {
		t.Fatalf("workspace offline on: %v (%s)", err, out)
	}
	if !strings.Contains(out, "offline providers: on") {
		t.Fatalf("offline output = %q, want the on posture", out)
	}

	st2, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st2.Close() }()

	cold, err := st2.WorkspaceByName(ctx, "cold")
	if err != nil {
		t.Fatalf("by name cold: %v", err)
	}
	if !cold.OfflineProviders {
		t.Fatalf("cold offline = false, want true")
	}
	edge, err := st2.WorkspaceByName(ctx, "edge")
	if err != nil {
		t.Fatalf("by name edge: %v", err)
	}
	if edge.OfflineProviders {
		t.Fatalf("sibling edge offline = true, want false")
	}

	out2, err := runCobra(t, newWorkspaceCmd, "offline", "cold", "off")
	if err != nil {
		t.Fatalf("workspace offline off: %v (%s)", err, out2)
	}
	st3, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("reopen store 2: %v", err)
	}
	defer func() { _ = st3.Close() }()
	cold2, err := st3.WorkspaceByName(ctx, "cold")
	if err != nil {
		t.Fatalf("by name cold 2: %v", err)
	}
	if cold2.OfflineProviders {
		t.Fatalf("cold offline still true after off, want false")
	}
}

// TestWorkspaceOfflineRefusesJunkToggle: a toggle that is not on or off is
// refused at the CLI, so a typo cannot silently flip a posture.
func TestWorkspaceOfflineRefusesJunkToggle(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateWorkspace(ctx, store.Workspace{Name: "cold", TofuDir: "/srv/cold"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = st.Close()

	if _, err := runCobra(t, newWorkspaceCmd, "offline", "cold", "maybe"); err == nil {
		t.Fatal("offline maybe was accepted, want a toggle refusal")
	}
}
