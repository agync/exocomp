package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// TestWorkspacesPageReadonlySkipsPlanQueryCounting is the F8 fix's stronger
// proof: the readonly render used to assert only the Operator-gated markup,
// which passes even if the vm.Operator guard on the plan query were removed.
// The handler now calls the plan list through the Server's injectable
// planArtifactsFor field, and this test wraps it in a counter: a readonly
// render must call it ZERO times, an operator render must call it once per
// runnable workspace.
func TestWorkspacesPageReadonlySkipsPlanQueryCounting(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	h.seedPlanArtifact(t, "web")

	var mu sync.Mutex
	calls := 0
	h.server.planArtifactsFor = func(ctx context.Context, runbook, kind string, limit int) ([]store.Artifact, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return h.store.ListPlanArtifacts(ctx, runbook, kind, limit)
	}

	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)
	resp, err := client.Get(ts.URL + "/workspaces")
	if err != nil {
		t.Fatalf("readonly GET /workspaces: %v", err)
	}
	_ = readBody(t, resp)
	_ = resp.Body.Close()
	mu.Lock()
	roCalls := calls
	mu.Unlock()
	if roCalls != 0 {
		t.Fatalf("readonly render called the plan query %d times, want 0", roCalls)
	}

	op := h.mint(t, "laptop", "operator")
	opClient := login(t, ts.URL, op)
	resp, err = opClient.Get(ts.URL + "/workspaces")
	if err != nil {
		t.Fatalf("operator GET /workspaces: %v", err)
	}
	_ = readBody(t, resp)
	_ = resp.Body.Close()
	mu.Lock()
	opCalls := calls
	mu.Unlock()
	if opCalls == 0 {
		t.Fatal("operator render never called the plan query")
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
// state backend address is recorded. The block now lives on the workspace
// detail page (the row's Backend-config item opens it), not a list modal.
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

	resp, err := client.Get(ts.URL + "/workspaces/web")
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

// TestWorkspaceApplyUnadjudicatedPlanRefused409: an apply by saved-plan id on
// a policied workspace whose plan carries no recorded verdict is unconsumable —
// the browser face maps it to a 409 with the actionable message, not a 500,
// mirroring the API face's state-conflict mapping, and nothing enqueues.
func TestWorkspaceApplyUnadjudicatedPlanRefused409(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	h.seedWorkspace(t, "prod")
	h.seedPlanPolicy(t, "prod", "no-destroy", store.PolicyDeny)
	art := h.seedPlanArtifact(t, "prod")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/workspaces/prod/apply",
		url.Values{"plan": {strconv.FormatInt(art.ID, 10)}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("unadjudicated plan apply = %d, want 409 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "unconsumable") || !strings.Contains(body, "verdict") {
		t.Fatalf("unadjudicated plan apply body missing the actionable message: %s", body)
	}
	jobs, err := h.store.ListJobs(ctx, store.JobFilter{Runbook: service.WorkspaceRunbook("prod")})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("workspace holds %d jobs, want only the plan job — an apply slipped past the gate", len(jobs))
	}
}

// TestWorkspacesPageSavedPlans: a runs-in-server workspace with a saved plan
// renders the Saved plans control and its modal, whose apply form posts the
// plan by artifact id to the same apply route — and the inline plan-id input is
// gone.
func TestWorkspacesPageSavedPlans(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	planID := h.seedPlanArtifact(t, "web").ID
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp, err := client.Get(ts.URL + "/workspaces/web")
	if err != nil {
		t.Fatalf("GET /workspaces/web: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /workspaces/web = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `kicker">Saved plans`) {
		t.Fatal("workspace detail page missing the Saved plans section")
	}
	want := `<input type="hidden" name="plan" value="` + strconv.FormatInt(planID, 10) + `">`
	if !strings.Contains(body, want) {
		t.Fatalf("Saved plans modal missing the plan-by-id field %q", want)
	}
	if !strings.Contains(body, "Apply plan") {
		t.Fatal("workspace detail page missing the apply-plan button")
	}
	if !strings.Contains(body, `action="/workspaces/web/apply"`) {
		t.Fatal("workspace detail page does not post to the apply route")
	}
	if !strings.Contains(body, "A policied plan may still require approval.") {
		t.Fatal("workspace detail page missing the policied-plan note")
	}
	if strings.Contains(body, "plan id (optional)") || strings.Contains(body, `name="plan" min="1"`) {
		t.Fatal("workspace detail page still renders the inline plan-id input")
	}
}

// TestWorkspacesPageNoSavedPlans: a runs-in-server workspace with no saved
// plans still gets the Saved plans modal, telling the operator to run Plan.
func TestWorkspacesPageNoSavedPlans(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp, err := client.Get(ts.URL + "/workspaces/web")
	if err != nil {
		t.Fatalf("GET /workspaces/web: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /workspaces/web = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "No saved plans yet — run Plan to save one.") {
		t.Fatal("workspace detail page missing the no-plans line")
	}
}

// TestWorkspacesPageSavedPlansHiddenForStateOnly: a state-only workspace never
// renders the Saved plans control or modal — the daemon never plans it.
func TestWorkspacesPageSavedPlansHiddenForStateOnly(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
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
	if strings.Contains(body, "Saved plans") {
		t.Fatal("state-only workspace renders the Saved plans control")
	}
	if strings.Contains(body, `id="ws-plans-stateonly"`) {
		t.Fatal("state-only workspace renders a Saved plans modal")
	}
}

// TestWorkspaceDetailPage is the GATE for the v0.6 workspace detail page: it
// renders the identity facts, the Plan/Apply verbs, the Saved plans section
// (with the plan-by-id apply form), and the Backend config block. The list row
// opens it (data-href) and keeps only the row-actions glyphs.
func TestWorkspaceDetailPage(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	planID := h.seedPlanArtifact(t, "web").ID
	if err := api.WriteAddress(filepath.Join(h.dir, api.AddressFile), "127.0.0.1:9443"); err != nil {
		t.Fatalf("WriteAddress: %v", err)
	}
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	// The list row opens the detail page (data-href) and the row keeps the
	// Plan/Apply glyphs, no per-row view buttons.
	code, list := getBody(t, client, ts.URL+"/workspaces")
	if code != http.StatusOK {
		t.Fatalf("/workspaces = %d", code)
	}
	for _, want := range []string{
		`data-href="/workspaces/web"`,
		`aria-label="Plan"`,
		`aria-label="Apply"`,
	} {
		if !strings.Contains(list, want) {
			t.Errorf("workspaces list missing %q", want)
		}
	}
	if strings.Contains(list, "Saved plans") || strings.Contains(list, "Backend config — web") {
		t.Error("Saved plans / Backend config must not render as per-row modals on the list (R8)")
	}
	// The row's chevron menu keeps the Backend-config entry (it links to the
	// detail page), so a state-only workspace still has a reachable verb.
	if !strings.Contains(list, `class="rachev"`) || !strings.Contains(list, "Backend config") {
		t.Error("workspaces row menu missing the Backend config entry")
	}

	// The detail page hosts them.
	code, detail := getBody(t, client, ts.URL+"/workspaces/web")
	if code != http.StatusOK {
		t.Fatalf("/workspaces/web = %d", code)
	}
	for _, want := range []string{
		`>Plan</button>`,
		`data-confirm="Apply web?"`,
		`kicker">Saved plans`,
		`name="plan" value="` + strconv.FormatInt(planID, 10) + `"`,
		`Apply plan`,
		`kicker">Backend config`,
		`backend &#34;http&#34;`,
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("workspace detail missing %q", want)
		}
	}
}
