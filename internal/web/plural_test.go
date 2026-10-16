package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestPlural pins the helper every count-bearing noun reads.
func TestPlural(t *testing.T) {
	cases := []struct {
		n         int
		one, many string
		want      string
	}{
		{0, "serial", "serials", "serials"},
		{1, "serial", "serials", "serial"},
		{2, "serial", "serials", "serials"},
		{1, "plan", "plans", "plan"},
		{3, "plan", "plans", "plans"},
	}
	for _, c := range cases {
		if got := plural(c.n, c.one, c.many); got != c.want {
			t.Errorf("plural(%d, %q, %q) = %q, want %q", c.n, c.one, c.many, got, c.want)
		}
	}
}

// TestWorkspaceSerialCountsSingular is the regression gate for the "1 serials"
// defect: one stored serial must read "1 serial" on the list line and on the
// §2.1 Tofu state tile, and must never read "1 serials".
func TestWorkspaceSerialCountsSingular(t *testing.T) {
	r := workspaceWriteRow{Serial: 1, SerialCount: 1, StateSize: "1.9 KB"}
	if got, want := r.StateLine(), "serial 1 · 1 serial · 1.9 KB"; got != want {
		t.Errorf("StateLine() = %q, want %q", got, want)
	}
	r.SerialCount = 3
	if got := r.StateLine(); got != "serial 1 · 3 serials · 1.9 KB" {
		t.Errorf("StateLine() with 3 serials = %q", got)
	}

	vm := workspaceDetailVM{StateTray: make([]stateTrayRow, 1)}
	if got, want := vm.TileState(), "1 serial"; got != want {
		t.Errorf("TileState() = %q, want %q", got, want)
	}
	vm.StateTray = make([]stateTrayRow, 4)
	if got, want := vm.TileState(), "4 serials"; got != want {
		t.Errorf("TileState() with 4 = %q, want %q", got, want)
	}
}

// TestWorkspaceDetailSingleSerialRenders is the render-level gate: a workspace
// with exactly one stored serial renders the tray subtitle as "1 serial ·",
// not "1 serials ·".
func TestWorkspaceDetailSingleSerialRenders(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	ws := h.seedWorkspace(t, "solo")
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 1, Lineage: "abcd",
		Ciphertext: []byte("only state body"),
	}); err != nil {
		t.Fatalf("append tfstate: %v", err)
	}

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	code, body := getBody(t, client, ts.URL+"/workspaces/solo")
	if code != http.StatusOK {
		t.Fatalf("GET /workspaces/solo = %d (%s)", code, body)
	}
	if strings.Contains(body, "1 serials") {
		t.Errorf("workspace detail renders %q; singular must read \"1 serial\"", "1 serials")
	}
	if !strings.Contains(body, "1 serial") {
		t.Errorf("workspace detail does not render the singular \"1 serial\"")
	}
}
