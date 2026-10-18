package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// newSource registers a git source for the egress-default tests.
func newSource(t *testing.T, s *store.Store, name string) store.Source {
	t.Helper()
	src, err := s.CreateSource(context.Background(), store.Source{
		Name: name, Kind: store.SourceGit, Ref: "main", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create source %q: %v", name, err)
	}
	return src
}

func TestSourceEgressPortsDefaultEmpty(t *testing.T) {
	s, _ := open(t)
	src := newSource(t, s, "edge")
	if len(src.ExtraEgressPorts) != 0 {
		t.Fatalf("new source egress ports = %v, want empty", src.ExtraEgressPorts)
	}
	// A round-trip through the store must not invent a declaration either.
	got, err := s.SourceByName(context.Background(), "edge")
	if err != nil {
		t.Fatalf("by name: %v", err)
	}
	if len(got.ExtraEgressPorts) != 0 {
		t.Fatalf("reloaded egress ports = %v, want empty", got.ExtraEgressPorts)
	}
}

func TestSetSourceEgressPortsPersistsPerSource(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	edge := newSource(t, s, "edge")
	sibling := newSource(t, s, "sibling")

	updated, err := s.SetSourceEgressPorts(ctx, edge.ID, []int{443, 443, 2222})
	if err != nil {
		t.Fatalf("set egress: %v", err)
	}
	if want := []int{443, 2222}; !slices.Equal(updated.ExtraEgressPorts, want) {
		t.Fatalf("egress ports = %v, want %v (deduped, sorted)", updated.ExtraEgressPorts, want)
	}

	// The declaration is scoped to the one source: a sibling that declared
	// nothing must still read back empty.
	got, err := s.GetSource(ctx, sibling.ID)
	if err != nil {
		t.Fatalf("get sibling: %v", err)
	}
	if len(got.ExtraEgressPorts) != 0 {
		t.Fatalf("sibling egress ports = %v, want empty", got.ExtraEgressPorts)
	}
}

func TestSetSourceEgressPortsRejectsJunk(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	src := newSource(t, s, "edge")

	for _, bad := range [][]int{{0}, {-1}, {65536}, {443, 70000}} {
		if _, err := s.SetSourceEgressPorts(ctx, src.ID, bad); !errors.Is(err, store.ErrInvalidEgressPort) {
			t.Fatalf("SetSourceEgressPorts(%v) err = %v, want ErrInvalidEgressPort", bad, err)
		}
	}
	// A rejected declaration must not have partially written.
	got, err := s.GetSource(ctx, src.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.ExtraEgressPorts) != 0 {
		t.Fatalf("after rejected writes egress ports = %v, want empty", got.ExtraEgressPorts)
	}
}

func TestSetSourceEgressPortsUnknownSource(t *testing.T) {
	s, _ := open(t)
	if _, err := s.SetSourceEgressPorts(context.Background(), 9999, []int{443}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
