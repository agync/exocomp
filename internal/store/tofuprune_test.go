package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestPruneTFStates proves the keep policy: old serials beyond the newest N are
// dropped, while history inside the time window — and the newest N regardless of
// age — is kept. The latest version is never lost.
func TestPruneTFStates(t *testing.T) {
	ctx := context.Background()

	appendN := func(t *testing.T, s *store.Store, ws store.Workspace, n int) {
		t.Helper()
		for i := 1; i <= n; i++ {
			if _, err := s.AppendTFState(ctx, store.NewTFState{
				WorkspaceID: ws.ID,
				SerialHint:  int64(i),
				Lineage:     "abcd",
				Ciphertext:  []byte(fmt.Sprintf(`{"serial":%d}`, i)),
			}); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		}
	}

	t.Run("old serials beyond the window drop to the newest N", func(t *testing.T) {
		s, _ := open(t)
		ws := newWorkspace(t, s, "aged")
		appendN(t, s, ws, 5)

		// Every version is older than a cutoff in the future, so only the serial
		// window protects them: the newest 2 survive, the older 3 are pruned.
		pruned, err := s.PruneTFStates(ctx, ws.ID, 2, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("prune: %v", err)
		}
		if pruned != 3 {
			t.Fatalf("pruned %d, want 3", pruned)
		}
		got, err := s.ListTFStates(ctx, ws.ID, 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 || got[0].Serial != 5 || got[1].Serial != 4 {
			t.Fatalf("kept = %+v, want serials 5 and 4", serials(got))
		}
		if latest, err := s.LatestTFState(ctx, ws.ID); err != nil || latest.Serial != 5 {
			t.Fatalf("latest = %d (%v), want serial 5", latest.Serial, err)
		}
	})

	t.Run("history inside the window is kept regardless of serial count", func(t *testing.T) {
		s, _ := open(t)
		ws := newWorkspace(t, s, "fresh")
		appendN(t, s, ws, 5)

		// A cutoff in the past leaves every version inside the window, so a tight
		// serial budget prunes nothing.
		pruned, err := s.PruneTFStates(ctx, ws.ID, 1, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatalf("prune: %v", err)
		}
		if pruned != 0 {
			t.Fatalf("pruned %d, want 0 (all inside the window)", pruned)
		}
		if got, err := s.ListTFStates(ctx, ws.ID, 0); err != nil || len(got) != 5 {
			t.Fatalf("kept %d, want 5", len(got))
		}
	})

	t.Run("keepSerials below one still keeps the latest", func(t *testing.T) {
		s, _ := open(t)
		ws := newWorkspace(t, s, "atleastone")
		appendN(t, s, ws, 3)

		pruned, err := s.PruneTFStates(ctx, ws.ID, 0, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("prune: %v", err)
		}
		if pruned != 2 {
			t.Fatalf("pruned %d, want 2", pruned)
		}
		if latest, err := s.LatestTFState(ctx, ws.ID); err != nil || latest.Serial != 3 {
			t.Fatalf("latest = %d (%v), want serial 3", latest.Serial, err)
		}
	})
}

func serials(v []store.TFState) []int64 {
	out := make([]int64, len(v))
	for i := range v {
		out[i] = v[i].Serial
	}
	return out
}
