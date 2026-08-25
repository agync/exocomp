package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

func TestWorkspaceWriteReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/workspaces/web/plan", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly workspace plan = %d, want 403", resp.StatusCode)
	}
}

func TestWorkspaceWriteCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/workspaces/web/plan", url.Values{},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin workspace plan = %d, want 403", resp.StatusCode)
	}
}

// TestWorkspacePlanEnqueues: an operator's browser-form plan on a seeded
// workspace enqueues a job through the SAME service.PlanWorkspace verb the
// bearer-token API face calls, and redirects to that job's page.
func TestWorkspacePlanEnqueues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/workspaces/web/plan", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("workspace plan = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("workspace plan did not redirect to a job page")
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: service.WorkspaceRunbook("web")})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) == 0 {
		t.Fatal("workspace plan did not enqueue a job through the service verb")
	}
	if jobs[0].ConcurrencyKey != "tofu:web" {
		t.Fatalf("job concurrency_key = %q, want tofu:web", jobs[0].ConcurrencyKey)
	}
}

// TestWorkspaceApplyFreshEnqueues: a fresh apply (no plan id) on an unpoliced
// workspace auto-approves and enqueues plainly, mirroring apiWorkspaceApply's
// saved==nil path.
func TestWorkspaceApplyFreshEnqueues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/workspaces/web/apply", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("workspace apply = %d, want 303", resp.StatusCode)
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: service.WorkspaceRunbook("web")})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) == 0 {
		t.Fatal("workspace apply did not enqueue a job through the service verb")
	}
}

// TestWorkspacesPageListsSeeded confirms the new GET /workspaces page renders
// a seeded workspace's name for both operator and readonly identities (the
// latter read-only, no controls).
func TestWorkspacesPageListsSeeded(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp, err := client.Get(ts.URL + "/workspaces")
	if err != nil {
		t.Fatalf("GET /workspaces: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /workspaces = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "web") {
		t.Fatal("workspaces page did not list the seeded workspace")
	}
}

// TestWorkspacesPageLabelledOpenTofuState: the surface's user-facing label is
// "Workspaces" (the h1/title), even though the surface manages OpenTofu state
// "workspaces" — the DR-critical store/API name is unchanged (AGENTS.md).
func TestWorkspacesPageLabelledOpenTofuState(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp, err := client.Get(ts.URL + "/workspaces")
	if err != nil {
		t.Fatalf("GET /workspaces: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /workspaces = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Workspaces") {
		t.Fatal("workspaces page does not render the Workspaces label")
	}
	if !strings.Contains(body, "<title>Workspaces") {
		t.Fatalf("workspaces page title not set to Workspaces: %s", body)
	}
}

// TestWorkspacesPageDistinguishesType: a state-only row (empty TofuDir) shows
// as "state-only" and a runs-in-server row (TofuDir set) shows as "runs in
// server" — the reframed Type column.
func TestWorkspacesPageDistinguishesType(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "runsinserver")
	if _, err := h.store.CreateWorkspace(context.Background(), store.Workspace{Name: "stateonly"}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp, err := client.Get(ts.URL + "/workspaces")
	if err != nil {
		t.Fatalf("GET /workspaces: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /workspaces = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "state-only") {
		t.Fatal("workspaces page did not mark the state-only row")
	}
	if !strings.Contains(body, "runs in server") {
		t.Fatal("workspaces page did not mark the runs-in-server row")
	}
}

// TestWorkspacesPageBackendConfigModal: the per-row backend-config modal
// carries a ready-to-paste `backend "http"` block, computed the same way the
// CLI's `workspace add --state-only` computes it (api.NewBackendBlock) once a
// state backend address is recorded.
func TestWorkspacesPageBackendConfigModal(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	if err := api.WriteAddress(filepath.Join(h.dir, api.AddressFile), "127.0.0.1:9443"); err != nil {
		t.Fatalf("WriteAddress: %v", err)
	}
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp, err := client.Get(ts.URL + "/workspaces")
	if err != nil {
		t.Fatalf("GET /workspaces: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /workspaces = %d, want 200 (%s)", resp.StatusCode, body)
	}
	// The page HTML-escapes the block's text content, so the quoted
	// `backend "http"` reads as `backend &#34;http&#34;` on the wire; check for
	// that plus the surrounding HCL shape rather than the raw Go string.
	if !strings.Contains(body, "backend &#34;http&#34;") {
		t.Fatal("workspaces page did not render a backend config block")
	}
	if !strings.Contains(body, "lock_address") || !strings.Contains(body, "Authorization") {
		t.Fatal("backend config block missing lock_address or Authorization header")
	}
	want := api.NewBackendBlock(h.server.deriver, "127.0.0.1:9443", "web")
	if !strings.Contains(body, want.Address) {
		t.Fatal("backend config block did not carry the computed state address")
	}
	if !strings.Contains(body, want.Token) {
		t.Fatal("backend config block did not carry the computed bearer")
	}
}

// TestWorkspacesPageConfigure: the header's Configure action opens a modal
// titled "Configure remote state backend" whose OpenTofu/Terraform tabs carry
// the `backend "http"` block at the backend's base address and the init step;
// the list section is titled "States"; the old always-visible how-to card is
// gone.
func TestWorkspacesPageConfigure(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	if err := api.WriteAddress(filepath.Join(h.dir, api.AddressFile), "127.0.0.1:9443"); err != nil {
		t.Fatalf("WriteAddress: %v", err)
	}
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp, err := client.Get(ts.URL + "/workspaces")
	if err != nil {
		t.Fatalf("GET /workspaces: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /workspaces = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Configure remote state backend") {
		t.Fatal("workspaces page missing the Configure modal title")
	}
	if !strings.Contains(body, `data-modal-open="ws-configure"`) {
		t.Fatal("workspaces page missing the Configure button")
	}
	if !strings.Contains(body, ">OpenTofu<") || !strings.Contains(body, ">Terraform<") {
		t.Fatal("Configure modal missing the OpenTofu/Terraform tabs")
	}
	if !strings.Contains(body, "backend &#34;http&#34;") {
		t.Fatal("Configure modal did not render a backend config snippet")
	}
	if !strings.Contains(body, "tofu init") || !strings.Contains(body, "terraform init") {
		t.Fatal("Configure modal missing the init step")
	}
	if !strings.Contains(body, ">States<") {
		t.Fatal("workspaces list section is not titled States")
	}
	if strings.Contains(body, "Using exocomp as your OpenTofu/Terraform state backend") {
		t.Fatal("workspaces page still renders the removed how-to card")
	}
}
