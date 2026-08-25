package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/naming"
	"exocomp.dev/exocomp/internal/store"
)

// unsafeNames are the shapes finding #2 flagged: a crafted workspace or
// source name that is not a single safe path component. Each must be refused
// at creation, before it ever reaches a filepath.Join sink (the module cache,
// the inventory handoff path, the plan -out path, the source checkout root).
var unsafeNames = []struct {
	label, name string
}{
	{"parent-traversal", "../.."},
	{"nested-separator", "a/b/c"},
	{"dot", "."},
	{"dotdot", ".."},
	{"empty", ""},
	{"nul-byte", "evil\x00name"},
}

// TestWorkspaceAddRefusesUnsafeNames proves workspace add refuses a
// traversal-shaped name before creating anything, closing finding #2 at the
// boundary rather than trusting a downstream filepath.Join to survive it.
func TestWorkspaceAddRefusesUnsafeNames(t *testing.T) {
	for _, tc := range unsafeNames {
		t.Run(tc.label, func(t *testing.T) {
			dir := initForSource(t)
			tofuDir := t.TempDir()

			out, err := runCobra(t, newWorkspaceCmd, "add", tc.name, "--dir", tofuDir)
			if err == nil {
				t.Fatalf("workspace add %q: want error, got success: %s", tc.name, out)
			}

			st, serr := store.Open(filepath.Join(dir, "exocomp.db"))
			if serr != nil {
				t.Fatalf("open store: %v", serr)
			}
			defer func() { _ = st.Close() }()
			list, lerr := st.ListWorkspaces(context.Background())
			if lerr != nil {
				t.Fatalf("list workspaces: %v", lerr)
			}
			if len(list) != 0 {
				t.Fatalf("workspace add %q: created %d workspace rows, want none: %+v",
					tc.name, len(list), list)
			}
		})
	}
}

// TestWorkspaceAddStateOnlyRefusesUnsafeNames covers the second CreateWorkspace
// call site (mode A / --state-only), which shares the same downstream sinks
// (the inventory handoff path keys off the workspace name too).
func TestWorkspaceAddStateOnlyRefusesUnsafeNames(t *testing.T) {
	for _, tc := range unsafeNames {
		t.Run(tc.label, func(t *testing.T) {
			dir := initForSource(t)

			out, err := runCobra(t, newWorkspaceCmd, "add", tc.name,
				"--state-only", "--address", "127.0.0.1:1")
			if err == nil {
				t.Fatalf("workspace add --state-only %q: want error, got success: %s", tc.name, out)
			}

			st, serr := store.Open(filepath.Join(dir, "exocomp.db"))
			if serr != nil {
				t.Fatalf("open store: %v", serr)
			}
			defer func() { _ = st.Close() }()
			list, lerr := st.ListWorkspaces(context.Background())
			if lerr != nil {
				t.Fatalf("list workspaces: %v", lerr)
			}
			if len(list) != 0 {
				t.Fatalf("workspace add --state-only %q: created %d workspace rows, want none",
					tc.name, len(list))
			}
		})
	}
}

// TestWorkspaceAddAcceptsSafeName proves the guard is not overzealous: a
// normal name still works, and the row it creates is a safe path component
// (so every downstream filepath.Join sink stays safe on the strength of this
// one check).
func TestWorkspaceAddAcceptsSafeName(t *testing.T) {
	dir := initForSource(t)
	tofuDir := t.TempDir()

	out, err := runCobra(t, newWorkspaceCmd, "add", "prod-net", "--dir", tofuDir)
	if err != nil {
		t.Fatalf("workspace add: %v (%s)", err, out)
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	ws, err := st.WorkspaceByName(context.Background(), "prod-net")
	if err != nil {
		t.Fatalf("workspace by name: %v", err)
	}
	if !naming.Component(ws.Name) {
		t.Fatalf("created workspace name %q is not a safe path component", ws.Name)
	}
}

// TestSourceAddRefusesUnsafeNames covers both source add branches (--path and
// --url): a crafted name must not reach source.Resolve's checkout-root join.
func TestSourceAddRefusesUnsafeNames(t *testing.T) {
	for _, tc := range unsafeNames {
		t.Run(tc.label+"/path", func(t *testing.T) {
			dir := initForSource(t)
			out, err := runSource(t, "add", tc.name, "--path", "/mnt/x")
			if err == nil {
				t.Fatalf("source add %q --path: want error, got success: %s", tc.name, out)
			}
			assertNoSourceCreated(t, dir, tc.name)
		})
		t.Run(tc.label+"/git", func(t *testing.T) {
			dir := initForSource(t)
			out, err := runSource(t, "add", tc.name, "--url", "git@forge:org/infra.git")
			if err == nil {
				t.Fatalf("source add %q --url: want error, got success: %s", tc.name, out)
			}
			assertNoSourceCreated(t, dir, tc.name)
		})
	}
}

func assertNoSourceCreated(t *testing.T, dataDir, name string) {
	t.Helper()
	st, err := store.Open(filepath.Join(dataDir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.SourceByName(context.Background(), name); err == nil {
		t.Fatalf("source add %q: a source row was created despite the refusal", name)
	} else if !errors.Is(err, store.ErrNotFound) {
		// Anything other than "no such row" is a store problem, not proof the
		// guard held; surface it rather than assuming absence.
		t.Fatalf("source by name %q: unexpected error: %v", name, err)
	}
}
