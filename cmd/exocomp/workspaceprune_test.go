package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestWorkspacePruneCommand proves the maintenance command applies the keep
// policy through the store: seeded serials past the budget and outside the
// window are dropped, the current state survives.
func TestWorkspacePruneCommand(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ws, err := st.CreateWorkspace(ctx, store.Workspace{Name: "net", TofuDir: "/srv/net"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := st.AppendTFState(ctx, store.NewTFState{
			WorkspaceID: ws.ID, SerialHint: int64(i), Lineage: "l",
			Ciphertext: []byte(fmt.Sprintf(`{"serial":%d}`, i)),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	_ = st.Close()

	// --days -1 puts the window cutoff in the future, so only the serial budget
	// protects versions: keep the newest 2, prune the older 3.
	out, err := runCobra(t, newWorkspaceCmd, "prune", "net", "--keep", "2", "--days", "-1")
	if err != nil {
		t.Fatalf("workspace prune: %v (%s)", err, out)
	}
	if !strings.Contains(out, "net") || !strings.Contains(out, "pruned 3") {
		t.Fatalf("prune output = %q, want a pruned-3 line", out)
	}

	st2, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st2.Close() }()
	got, err := st2.ListTFStates(ctx, ws.ID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("kept %d versions, want 2", len(got))
	}
	if latest, err := st2.LatestTFState(ctx, ws.ID); err != nil || latest.Serial != 5 {
		t.Fatalf("latest serial = %d (%v), want 5", latest.Serial, err)
	}
}
