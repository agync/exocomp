package checkprobe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestRunProbesAndWritesNothing: Run refuses the loopback class at the sweep's
// options (AllowLoopback=false), so a live loopback listener reads down with a
// refusal while a closed loopback port reads down from the dial; it honors the
// target narrowing and leaves the store exactly as it found it — no
// check_state row, no event, no check-row touch. It is the D3 guard for the
// shared runner.
func TestRunProbesAndWritesNothing(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	c, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "live", Address: "127.0.0.1", Port: port},
			{Name: "closed", Address: "127.0.0.1", Port: 1},
		},
	})
	if err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	before, err := st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}

	results, err := Run(ctx, st, c, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Run returned %d targets, want 2", len(results))
	}
	if results[0].Name != "live" || results[0].Up {
		t.Errorf("live loopback target = %+v, want down (guard refused)", results[0])
	}
	if !strings.Contains(results[0].Detail, "refusing") {
		t.Errorf("live loopback target detail = %q, want the refusal", results[0].Detail)
	}
	if results[1].Name != "closed" || results[1].Up {
		t.Errorf("closed target = %+v, want down", results[1])
	}
	if results[1].Detail == "" {
		t.Errorf("closed target carries no detail sentence: %+v", results[1])
	}

	narrowed, err := Run(ctx, st, c, "closed")
	if err != nil {
		t.Fatalf("Run narrow: %v", err)
	}
	if len(narrowed) != 1 || narrowed[0].Name != "closed" {
		t.Fatalf("narrowed = %+v, want just closed", narrowed)
	}

	if _, err := Run(ctx, st, c, "ghost"); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("Run ghost err = %v, want ErrNoTarget", err)
	}

	states, err := st.ListCheckStates(ctx, c.ID)
	if err != nil {
		t.Fatalf("ListCheckStates: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("Run wrote %d check_state rows", len(states))
	}
	events, err := st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("Run emitted %d events", len(events))
	}
	after, err := st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck after: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("Run touched the check row: %s -> %s", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestRunStoredNetgroupPrecedesLive: a target the sweep has observed carries the
// stored netgroup even when a live classification of its address would land
// elsewhere; an unobserved target falls back to the live classification.
func TestRunStoredNetgroupPrecedesLive(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.CreateNetgroup(ctx, store.NewNetgroup{
		Name:     "home",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	}); err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}
	c, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "observed", Address: "127.0.0.1", Port: 1},
			{Name: "fresh", Address: "127.0.0.1", Port: 2},
		},
	})
	if err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: c.ID, TargetName: "observed", Status: store.CheckDown,
		Consecutive: 3, Emitted: store.CheckUp, Netgroup: "away",
	}); err != nil {
		t.Fatalf("SetCheckState: %v", err)
	}

	results, err := Run(ctx, st, c, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	byName := map[string]Target{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if byName["observed"].Netgroup != "away" {
		t.Errorf("observed target netgroup = %q, want the stored away", byName["observed"].Netgroup)
	}
	if byName["fresh"].Netgroup != "home" {
		t.Errorf("fresh target netgroup = %q, want the live home", byName["fresh"].Netgroup)
	}
}
