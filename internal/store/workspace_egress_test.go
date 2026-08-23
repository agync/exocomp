package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

func TestWorkspaceEgressPortsDefaultEmpty(t *testing.T) {
	s, _ := open(t)
	ws := newWorkspace(t, s, "edge")
	if len(ws.ExtraEgressPorts) != 0 {
		t.Fatalf("new workspace egress ports = %v, want empty", ws.ExtraEgressPorts)
	}
	// A round-trip through the store must not invent a declaration either.
	got, err := s.WorkspaceByName(context.Background(), "edge")
	if err != nil {
		t.Fatalf("by name: %v", err)
	}
	if len(got.ExtraEgressPorts) != 0 {
		t.Fatalf("reloaded egress ports = %v, want empty", got.ExtraEgressPorts)
	}
}

func TestSetWorkspaceEgressPortsPersistsPerWorkspace(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	edge := newWorkspace(t, s, "edge")
	sibling := newWorkspace(t, s, "sibling")

	updated, err := s.SetWorkspaceEgressPorts(ctx, edge.ID, []int{6443, 6443, 443})
	if err != nil {
		t.Fatalf("set egress: %v", err)
	}
	if want := []int{443, 6443}; !slices.Equal(updated.ExtraEgressPorts, want) {
		t.Fatalf("egress ports = %v, want %v (deduped, sorted)", updated.ExtraEgressPorts, want)
	}

	// The declaration is scoped to the one workspace: a sibling that declared
	// nothing must still read back empty.
	got, err := s.GetWorkspace(ctx, sibling.ID)
	if err != nil {
		t.Fatalf("get sibling: %v", err)
	}
	if len(got.ExtraEgressPorts) != 0 {
		t.Fatalf("sibling egress ports = %v, want empty", got.ExtraEgressPorts)
	}
}

func TestSetWorkspaceEgressPortsRejectsJunk(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ws := newWorkspace(t, s, "edge")

	for _, bad := range [][]int{{0}, {-1}, {65536}, {443, 70000}} {
		if _, err := s.SetWorkspaceEgressPorts(ctx, ws.ID, bad); !errors.Is(err, store.ErrInvalidEgressPort) {
			t.Fatalf("SetWorkspaceEgressPorts(%v) err = %v, want ErrInvalidEgressPort", bad, err)
		}
	}
	// A rejected declaration must not have partially written.
	got, err := s.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.ExtraEgressPorts) != 0 {
		t.Fatalf("after rejected writes egress ports = %v, want empty", got.ExtraEgressPorts)
	}
}

func TestSetWorkspaceEgressPortsUnknownWorkspace(t *testing.T) {
	s, _ := open(t)
	if _, err := s.SetWorkspaceEgressPorts(context.Background(), 9999, []int{6443}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
