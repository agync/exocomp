package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// TestWorkspaceDetailStateTray is the GATE for the §2.3 Tofu state tray: it
// renders the workspace's tfstate serials from ListTFStates NEWEST-FIRST with
// the adoption as the FIRST serial, browses (show-json) only the serials that
// carry a sealed `tofu-plan-show` companion, and leaves a "no sealed diff" note
// on the others. The §2.1 identity card becomes a 4-tile grid whose Tofu state
// tile links to the tray.
func TestWorkspaceDetailStateTray(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	ws := h.seedWorkspace(t, "web")

	// A plan made against serial 3 captured a show-json companion (the m46
	// shape), so serial 3 alone browses to a diff.
	art := h.seedPlanDiffWithShow(t, "web", 3, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],"before":{"value":"old"},"after":{"value":"new"}}}
	]}`)
	// An apply job wrote serial 2 — a normal plan/apply serial with no
	// companion.
	applyJob, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook("web"), Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue apply job: %v", err)
	}
	// Serial 1 is the adoption: the first serial, written outside a diff.
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 1, Lineage: "abcd",
		Ciphertext: []byte("adoption state body"),
	}); err != nil {
		t.Fatalf("append adoption tfstate: %v", err)
	}
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 2, Lineage: "abcd",
		JobID: &applyJob.ID, Ciphertext: []byte("job state body"),
	}); err != nil {
		t.Fatalf("append job tfstate: %v", err)
	}
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 3, Lineage: "abcd",
		Ciphertext: []byte("show state body"),
	}); err != nil {
		t.Fatalf("append show tfstate: %v", err)
	}

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	code, body := getBody(t, client, ts.URL+"/workspaces/web")
	if code != http.StatusOK {
		t.Fatalf("GET /workspaces/web = %d (%s)", code, body)
	}

	// The §2.1 identity card is a 4-tile grid, and the Tofu state tile links
	// to the tray card.
	for _, want := range []string{
		"<span>Dir</span>",
		"<span>Backend</span>",
		"<span>Provider mirror</span>",
		"<span>Tofu state</span>",
		`href="#tofu-state"`,
		`id="tofu-state"`,
		"3 serials",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("workspace detail tiles missing %q", want)
		}
	}

	// The §2.3 tray renders the serials NEWEST-FIRST from the store, adoption
	// (serial 1) as the first serial of the lineage.
	for _, want := range []string{
		`kicker">Tofu state`,
		"WRITTEN BY",
		"ENCRYPTED AT REST",
		"workspace adopt · migrated from backend",
		"job #" + strconv.FormatInt(applyJob.ID, 10),
		"outside exocomp",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("state tray missing %q", want)
		}
	}
	// Newest first: serial 3, then 2, then the adoption serial 1.
	s3 := strings.Index(body, ">serial 3<")
	s2 := strings.Index(body, ">serial 2<")
	s1 := strings.Index(body, ">serial 1<")
	if s3 < 0 || s2 < 0 || s1 < 0 {
		t.Fatalf("state tray missing a serial row (3=%d 2=%d 1=%d)", s3, s2, s1)
	}
	if s3 >= s2 || s2 >= s1 {
		t.Errorf("state tray serials not newest-first: 3@%d 2@%d 1@%d", s3, s2, s1)
	}

	// Browse is ONLY where a sealed show companion exists: serial 3 links to
	// its plan's diff; serials 1 and 2 read "no sealed diff".
	href := "/workspaces/web/plans/" + strconv.FormatInt(art.ID, 10)
	if !strings.Contains(body, `aria-label="Browse serial 3 diff"`) {
		t.Error("state tray serial 3 missing the browse verb")
	}
	if !strings.Contains(body, `href="`+href+`"`) {
		t.Errorf("state tray browse missing href %q", href)
	}
	if strings.Count(body, "no sealed diff") != 2 {
		t.Errorf("state tray %d 'no sealed diff' notes, want 2 (serials 1, 2)", strings.Count(body, "no sealed diff"))
	}
	// Read-only: no rollback verb exists.
	if strings.Contains(body, "roll back") || strings.Contains(body, "Roll back") {
		t.Error("state tray offers a rollback verb — the lane is read-only")
	}
	// The show-json diff for the browsed serial renders on its plan page.
	code, diff := getBody(t, client, ts.URL+"/workspaces/web/plans/"+strconv.FormatInt(art.ID, 10))
	if code != http.StatusOK {
		t.Fatalf("browse diff = %d (%s)", code, diff)
	}
	if !strings.Contains(diff, "kubernetes_config_map.marker") {
		t.Error("browsed show-json diff missing the changed resource")
	}
}

// TestWorkspaceDetailStateTrayEmpty: a workspace with no stored state renders
// the tray's empty note, never a bare empty table.
func TestWorkspaceDetailStateTrayEmpty(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	code, body := getBody(t, client, ts.URL+"/workspaces/web")
	if code != http.StatusOK {
		t.Fatalf("GET /workspaces/web = %d (%s)", code, body)
	}
	if !strings.Contains(body, "No state stored yet") {
		t.Errorf("state tray empty note missing (want %q)", "No state stored yet")
	}
	if strings.Contains(body, ">serial 1<") {
		t.Error("state tray rendered a serial for an empty workspace")
	}
}

// TestWorkspaceDetailStateTrayReadonly: the tray is a read-only surface shown
// to a readonly session too — no rollback, no gate on operator scope.
func TestWorkspaceDetailStateTrayReadonly(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	ws := h.seedWorkspace(t, "web")
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, Ciphertext: []byte("state"),
	}); err != nil {
		t.Fatalf("AppendTFState: %v", err)
	}
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)
	code, body := getBody(t, client, ts.URL+"/workspaces/web")
	if code != http.StatusOK {
		t.Fatalf("GET /workspaces/web = %d (%s)", code, body)
	}
	if !strings.Contains(body, `id="tofu-state"`) {
		t.Error("readonly session missing the state tray")
	}
}
