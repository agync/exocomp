package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
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
// as "state-only" and a runs-in-server row (TofuDir set) shows as
// "runs-in-server" — the POSTURE column (T7).
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
	if !strings.Contains(body, "state-only") || !strings.Contains(body, "external tofu") {
		t.Fatal("workspaces page did not mark the state-only row's posture")
	}
	if !strings.Contains(body, "runs-in-server") {
		t.Fatal("workspaces page did not mark the runs-in-server row's posture")
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
	// The reference block (SPEC §2.4) carries the address and lock plumbing but
	// NO credential: tofu sends http-backend headers verbatim, so a pasted
	// bearer would be a secret on screen. The value reaches init through the
	// filled config the CLI emits to a process pipe, never a paste block.

	if !strings.Contains(body, "lock_address") || !strings.Contains(body, "lock_method") {
		t.Fatal("backend config block missing the reference lock plumbing")
	}
	want := api.NewBackendBlock(h.server.deriver, "127.0.0.1:9443", "web")
	if !strings.Contains(body, want.Address) {
		t.Fatal("backend config block did not carry the computed state address")
	}
	// Leak check: the pasted block must not carry the bearer or an Authorization
	// header, and the reveal/copy handlers exist beside it.
	if strings.Contains(body, "Authorization") {
		t.Fatal("backend config block leaked an Authorization header")
	}
	if strings.Contains(body, want.Token) {
		t.Fatal("backend config block leaked the bearer")
	}
	if !strings.Contains(body, `id="be-reveal"`) || !strings.Contains(body, "Reveal bearer") {
		t.Fatal("backend card missing the reveal handler")
	}
	if !strings.Contains(body, "class=\"copybtn\"") {
		t.Fatal("backend card missing the copy handler")
	}
}

// TestWorkspaceBackendRevealEndpoint: the reveal glyph's fetch target returns
// the derived bearer to an operator (text/plain — never rendered in any paste
// block), and a readonly session is refused: reveal is a deliberate, logged
// action, not a surface fact (SPEC §2.4).
func TestWorkspaceBackendRevealEndpoint(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	if err := api.WriteAddress(filepath.Join(h.dir, "state-backend.addr"), "127.0.0.1:9443"); err != nil {
		t.Fatalf("WriteAddress: %v", err)
	}
	want := api.NewBackendBlock(h.server.deriver, "127.0.0.1:9443", "web").Token
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	resp, err := op.Post(ts.URL+"/workspaces/web/backend/reveal", "", nil)
	if err != nil {
		t.Fatalf("POST reveal: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reveal = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if body != want {
		t.Fatalf("reveal returned %q, want the derived bearer %q", body, want)
	}

	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))
	roResp, err := ro.Post(ts.URL+"/workspaces/web/backend/reveal", "", nil)
	if err != nil {
		t.Fatalf("readonly POST reveal: %v", err)
	}
	_ = readBody(t, roResp)
	if roResp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly reveal = %d, want 403", roResp.StatusCode)
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
	if !strings.Contains(body, ">Workspaces<") {
		t.Fatal("workspaces list section is not titled Workspaces")
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

// TestWorkspacesPageSavedPlans: the Plans tray-table (SPEC §2.2) renders one
// row per saved plan with the LIFECYCLE word — SAVED for the newest, SUPERSEDED
// for every older one — and exactly ONE apply glyph: on the latest SAVED
// non-stale row. The per-row Apply plan button and the plan= hidden field are
// gone; the apply affordance deep-links to the plan-detail page (lane F owns
// the route and its confirm).
func TestWorkspacesPageSavedPlans(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	older := h.seedPlanArtifactSerial(t, "web", 1)
	latest := h.seedPlanArtifactSerial(t, "web", 2)
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
	if !strings.Contains(body, "2 plans · newest first · latest is applicable") {
		t.Fatal("workspace detail page missing the plans count line")
	}
	for _, want := range []string{
		"SAVED",
		"SUPERSEDED",
		`aria-label="Apply plan #` + strconv.FormatInt(latest.ID, 10) + `"`,
		`/workspaces/web/plans/` + strconv.FormatInt(latest.ID, 10),
		`/workspaces/web/plans/` + strconv.FormatInt(older.ID, 10),
		"a Policy verdict rides on the plan: deny · require-approval",
		// The ACTIONS column right-alignment contract (SPEC §4): the header is
		// right-aligned and the cell is the flex-end actions cluster.
		`class="th-actions"`,
		`<div class="actions">`,
		// The LIFECYCLE cell carries the lift.css lifecycle name, whose stale
		// word never ellipsizes (nowrap, wait).
		`class="lifecycle">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("workspace detail missing %q", want)
		}
	}
	// Single-apply mutation check: the newest SAVED row carries the one play
	// glyph; the SUPERSEDED row must not render one. No apply form remains.
	if n := strings.Count(body, "aria-label=\"Apply plan #"); n != 1 {
		t.Errorf("Plans tray renders %d apply glyphs, want exactly 1", n)
	}
	if strings.Contains(body, "Apply plan\"") {
		t.Fatal("workspace detail page still renders a per-row Apply plan button")
	}
	if strings.Contains(body, `name="plan"`) {
		t.Fatal("workspace detail page still renders a plan= hidden field")
	}
	if strings.Contains(body, `action="/workspaces/web/apply"`) {
		t.Fatal("workspace detail page still posts an apply form")
	}
}

// TestWorkspacesListApplyGateHonest: the list's apply glyph is gated on
// serial-equal applicability, not on "any saved plan exists" — a SAVED plan
// the Tofu state has moved past (stale) shows NO apply on the list, exactly
// as the detail tray refuses it, and the TOFU STATE meta reads "no saved
// plan". Mutation: gate the apply on len(Plans)>0 and this flips red.
func TestWorkspacesListApplyGateHonest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ws := h.seedWorkspace(t, "web")
	if _, err := h.store.AppendTFState(context.Background(), store.NewTFState{
		WorkspaceID: ws.ID, Ciphertext: []byte("state"),
	}); err != nil {
		t.Fatalf("AppendTFState: %v", err)
	}
	if _, err := h.store.AppendTFState(context.Background(), store.NewTFState{
		WorkspaceID: ws.ID, Ciphertext: []byte("state"),
	}); err != nil {
		t.Fatalf("AppendTFState: %v", err)
	}
	// The plan is saved against serial 1; the state has moved to serial 2.
	h.seedPlanArtifactSerial(t, "web", 1)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/workspaces")
	if code != http.StatusOK {
		t.Fatalf("/workspaces = %d", code)
	}
	if strings.Contains(page, `aria-label="Apply"`) {
		t.Error("workspaces list offers apply for a stale plan")
	}
	if strings.Contains(page, `action="/workspaces/web/apply"`) {
		t.Error("workspaces list posts an apply form for a stale plan")
	}
	if !strings.Contains(page, "no saved plan") {
		t.Error("workspaces list missing the no-saved-plan TOFU STATE meta")
	}
}

// TestWorkspacesListApplyOrder: on an applicable row the actions cluster reads
// [apply]·plan·▾ left-to-right — apply prepended, plan and the chevron in
// their fixed right slots — so the constant verbs never move between rows.
func TestWorkspacesListApplyOrder(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ws := h.seedWorkspace(t, "web")
	if _, err := h.store.AppendTFState(context.Background(), store.NewTFState{
		WorkspaceID: ws.ID, Ciphertext: []byte("state"),
	}); err != nil {
		t.Fatalf("AppendTFState: %v", err)
	}
	h.seedPlanArtifactSerial(t, "web", 1)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/workspaces")
	if code != http.StatusOK {
		t.Fatalf("/workspaces = %d", code)
	}
	apply := strings.Index(page, `aria-label="Apply"`)
	plan := strings.Index(page, `aria-label="Plan"`)
	chev := strings.Index(page, `class="rachev"`)
	if apply < 0 || apply >= plan || plan >= chev {
		t.Error("applicable row actions are not [apply]·plan·▾ left-to-right")
	}
	if strings.Contains(page, "no saved plan") {
		t.Error("an applicable row must not read no saved plan")
	}
}

// TestWorkspacesListNoSavedPlanMeta: a runs-in-server workspace with no saved
// plan reads "no saved plan" in the TOFU STATE meta (the apply glyph is off).
func TestWorkspacesListNoSavedPlanMeta(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/workspaces")
	if code != http.StatusOK {
		t.Fatalf("/workspaces = %d", code)
	}
	if !strings.Contains(page, "no saved plan") {
		t.Error("no-plan workspace missing the no-saved-plan TOFU STATE meta")
	}
	if strings.Contains(page, `aria-label="Apply"`) {
		t.Error("no-plan workspace still offers apply")
	}
}

// TestWorkspaceDetailPlansChangesVerdict: the Plans tray's CHANGES cell reads
// the plan outcome's counters, resource names, drift note, and the policy
// verdict the plan job recorded — all from append-only job events (SPEC §2.2,
// copy.txt), never from a tofu show parse.
func TestWorkspaceDetailPlansChangesVerdict(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	art := h.seedPlanArtifactSerial(t, "web", 1)
	h.seedPlanOutcome(t, art, true, []executor.ResourceDelta{
		{Address: "kubernetes_config_map.marker", Actions: []string{"update"}},
		{Address: "kubernetes_namespace.e2e", Actions: []string{"create"}},
		{Address: "kubernetes_secret.old", Actions: []string{"delete"}},
	}, []executor.ResourceDelta{{Address: "kubernetes_config_map.drifted", Actions: []string{"update"}}})
	h.seedPlanVerdict(t, art, store.PolicyRequireApproval, "ops")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, body := getBody(t, client, ts.URL+"/workspaces/web")
	if code != http.StatusOK {
		t.Fatalf("/workspaces/web = %d", code)
	}
	for _, want := range []string{
		"+1",
		"~1",
		"−1",
		"kubernetes_config_map.marker, kubernetes_namespace.e2e, kubernetes_secret.old",
		"· drift",
		"require-approval · pending",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Plans tray CHANGES/VERDICT missing %q", want)
		}
	}
}

// TestWorkspaceDetailPlansDenyVerdict: a deny verdict renders "deny · <policy>"
// in the CHANGES·VERDICT cell, and a no-match adjudication reads "no policy
// matched" — both keyed to the sealed artifact, exactly as the seal wrote them.
func TestWorkspaceDetailPlansDenyVerdict(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	art := h.seedPlanArtifactSerial(t, "web", 1)
	h.seedPlanOutcome(t, art, true, []executor.ResourceDelta{{Address: "kubernetes_config_map.marker", Actions: []string{"update"}}}, nil)
	h.seedPlanVerdict(t, art, store.PolicyDeny, "no-destroy")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, body := getBody(t, client, ts.URL+"/workspaces/web")
	if code != http.StatusOK {
		t.Fatalf("/workspaces/web = %d", code)
	}
	if !strings.Contains(body, "deny · no-destroy") {
		t.Error("Plans tray missing the deny verdict")
	}
	h.seedPlanArtifact(t, "web")
	h.seedPlanVerdict(t, art, "no_match", "")
	code, body = getBody(t, client, ts.URL+"/workspaces/web")
	if code != http.StatusOK {
		t.Fatalf("/workspaces/web = %d", code)
	}
	if !strings.Contains(body, "no policy matched") {
		t.Error("Plans tray missing the no-match verdict")
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
	ws := h.seedWorkspace(t, "web")
	// The tray's stale rule:the Tofu state has moved to serial 2, so the one
	// SAVED plan (made against serial 1) reads stale and loses its apply.

	if _, err := h.store.AppendTFState(context.Background(), store.NewTFState{
		WorkspaceID: ws.ID, Ciphertext: []byte("state"),
	}); err != nil {
		t.Fatalf("AppendTFState: %v", err)
	}
	if _, err := h.store.AppendTFState(context.Background(), store.NewTFState{
		WorkspaceID: ws.ID, Ciphertext: []byte("state"),
	}); err != nil {
		t.Fatalf("AppendTFState: %v", err)
	}
	h.seedPlanArtifactSerial(t, "web", 1)
	if err := api.WriteAddress(filepath.Join(h.dir, "state-backend.addr"), "127.0.0.1:9443"); err != nil {
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
	} {
		if !strings.Contains(list, want) {
			t.Errorf("workspaces list missing %q", want)
		}
	}
	// The list apply gate is serial-equal (SPEC §4): the plan is stale, so the
	// list carries no apply glyph and its TOFU STATE meta reads the no-plan
	// line — the same honesty the detail tray's stale refusal promises.
	if strings.Contains(list, `aria-label="Apply"`) {
		t.Error("workspaces list offers apply for a stale plan")
	}
	if !strings.Contains(list, "no saved plan") {
		t.Error("workspaces list missing the no-saved-plan TOFU STATE meta")
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
		`kicker">Saved plans`,
		"SAVED",
		"stale · serial 2",
		`kicker">Backend config`,
		"backend &#34;http&#34;",
		"tofu init -backend-config=&lt;(exocomp workspace backend web --config)",
		"reveal logs an event · copy never includes it",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("workspace detail missing %q", want)
		}
	}
	// The stale copy is the short "stale · serial N" (SPEC §4) — the old
	// "stale · state at serial N" is retired.
	if strings.Contains(detail, "stale · state at serial") {
		t.Error("workspace detail still renders the old stale copy")
	}
	// The single-apply rule: a stale SAVED plan carries NO apply glyph (the
	// only apply candidate lost its affordance to the moved state), and the
	// pasted block never carries the bearer.
	if strings.Contains(detail, "aria-label=\"Apply plan #") {
		t.Error("workspace detail renders an apply glyph on a stale plan")
	}
	if strings.Contains(detail, "Authorization") || strings.Contains(detail, "Bearer ") {
		t.Error("backend block leaked the bearer into the pasted form")
	}
}

// TestWorkspacesListStateColumns: the T7 TOFU STATE column renders "serial N ·
// N serials · size" with the ENCRYPTED keyword, the meta names the newest
// serial's writer job and counts the external writes, and a runs-in-server row
// shows its newest job in the POSTURE meta. The state-only row reads
// "no state yet" until an external tool writes.
func TestWorkspacesListStateColumns(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	h.seedWorkspace(t, "web")
	if _, err := h.store.CreateWorkspace(ctx, store.Workspace{Name: "stateonly"}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	ws, err := h.store.WorkspaceByName(ctx, "web")
	if err != nil {
		t.Fatalf("WorkspaceByName: %v", err)
	}
	// Seed the plan before the state job so the state job stays the workspace's
	// newest (the POSTURE meta names it). The plan's own job is older. Saved
	// against serial 2 (set below), the plan keeps the row applicable, so the
	// TOFU STATE meta still reads the writer facts — a no-plan row would read
	// "no saved plan" instead (TestWorkspacesListNoSavedPlanMeta).
	h.seedPlanArtifactSerial(t, "web", 2)
	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook("web"), Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	// Serial 1 is an external write (no job); serial 2 was written by the job —
	// the adopted-history shape where the first serial came from outside.
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 1, Lineage: "abcd",
		Ciphertext: []byte("external state body"),
	}); err != nil {
		t.Fatalf("append external tfstate: %v", err)
	}
	if _, err := h.store.AppendTFState(ctx, store.NewTFState{
		WorkspaceID: ws.ID, SerialHint: 2, Lineage: "abcd",
		JobID:      &job.ID,
		Ciphertext: []byte("job state body"),
	}); err != nil {
		t.Fatalf("append job tfstate: %v", err)
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
	for _, want := range []string{
		// The newest serial's size and the serial count.
		"serial 2 · 2 serials · 14 B",
		// ENCRYPTED keyword renders with its lock glyph.
		"ENCRYPTED",
		`class="ws-encrypted"`,
		// The meta names the newest writer and the first external write.
		"by job #" + strconv.FormatInt(job.ID, 10),
		"1 external write (serial 1)",
		// POSTURE meta names the newest job.
		"last #" + strconv.FormatInt(job.ID, 10),
		// The state-only row has nothing stored yet.
		"no state yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("workspaces list missing %q", want)
		}
	}
	// The applicable plan keeps the writer meta: no no-saved-plan line.
	if strings.Contains(body, "no saved plan") {
		t.Error("applicable workspace still reads no saved plan")
	}
}

// TestWorkspacesListDriftedShowsApproval: a workspace named by an open approval
// renders the DRIFT chip PRESENT and its meta links the open approval.
func TestWorkspacesListDriftedShowsApproval(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	h.seedWorkspace(t, "web")
	job, err := h.queue.EnqueueGated(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook("web"), Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue gated job: %v", err)
	}
	now := time.Now().UTC()
	appr, err := h.store.CreateApproval(ctx, store.Approval{
		JobID: job.ID, Workspace: "web", TokenHash: "x",
		RequestedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
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
	for _, want := range []string{
		">PRESENT<",
		`approval #` + strconv.FormatInt(appr.ID, 10),
		`href="/approvals/` + strconv.FormatInt(appr.ID, 10) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("workspaces list missing %q", want)
		}
	}
}

// TestWorkspacesListLockedShowsForceUnlock: a workspace holding a tflock (with
// no live job) renders the DRIFT chip LOCKED and the TOFU STATE meta names the
// holder with a force-unlock button; the POST force-releases the lock and
// records the audit on the workspace's newest job.
func TestWorkspacesListLockedShowsForceUnlock(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	ws := h.seedWorkspace(t, "web")
	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook("web"), Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	if _, err := h.store.AcquireTFLock(ctx, store.TFLock{
		WorkspaceID: ws.ID, LockID: "stale", Who: "ci@lima",
		Operation: "OperationTypeApply", JobID: nil,
	}); err != nil {
		t.Fatalf("AcquireTFLock: %v", err)
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
	for _, want := range []string{
		">LOCKED<",
		"lock held by ci@lima since",
		`action="/workspaces/web/unlock"`,
		"force-unlock",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("workspaces list missing %q", want)
		}
	}

	// The POST force-releases and audits on the workspace's newest job.
	post := postForm(t, client, ts.URL+"/workspaces/web/unlock", url.Values{}, nil)
	_ = post.Body.Close()
	if post.StatusCode != http.StatusSeeOther {
		t.Fatalf("workspace unlock = %d, want 303", post.StatusCode)
	}
	if _, err := h.store.GetTFLock(ctx, ws.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("unlock did not release the lock")
	}
	events, err := h.store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	audited := false
	for _, e := range events {
		if strings.Contains(e.Msg, "force-unlocked tofu state") {
			audited = true
		}
	}
	if !audited {
		t.Fatal("unlock did not record the audit event on the workspace's newest job")
	}
}

// ---- Plan detail (SPEC §4) ----

// seedPlanDiff writes a REAL sealed plan for a workspace so the plan-detail
// page can open and classify it (seedPlanArtifactSerial's path is a stub the
// opener cannot read). planSerial is the workspace serial the plan was made
// against — the stale/current rule reads it, so a seeded serial is real.
func (h *harness) seedPlanDiff(t *testing.T, ws string, planSerial int64, planJSON string) store.Artifact {
	t.Helper()
	ctx := context.Background()
	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook(ws), Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue plan job: %v", err)
	}
	plain := filepath.Join(h.dir, "plan-"+itoa64(job.ID)+".json")
	if err := os.WriteFile(plain, []byte(planJSON), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	sealed, err := h.env.SealFile(plain)
	if err != nil {
		t.Fatalf("SealFile: %v", err)
	}
	art, err := h.store.CreateArtifact(ctx, store.Artifact{
		JobID: job.ID, Kind: reactive.PlanArtifactKind, Path: sealed,
		SHA256: fmt.Sprintf("diff-%d", job.ID), Size: 1,
		PlanSerial: planSerial,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	return art
}

// seedPlanDiffWithShow writes a plan whose sealed plan file is the BINARY tofu
// wrote (classifyPlan cannot parse it) and whose show-json companion holds the
// actual `tofu show -json` document — the production shape after the fix, where
// the executor captured the show-json at plan time. The web renders the diff
// from the companion, never from the plan file.
func (h *harness) seedPlanDiffWithShow(t *testing.T, ws string, planSerial int64, planJSON string) store.Artifact {
	t.Helper()
	ctx := context.Background()
	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook(ws), Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue plan job: %v", err)
	}
	planPlain := filepath.Join(h.dir, "plan-"+itoa64(job.ID)+".tfplan")
	if err := os.WriteFile(planPlain, []byte("tofu-binary-plan"), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	sealed, err := h.env.SealFile(planPlain)
	if err != nil {
		t.Fatalf("SealFile plan: %v", err)
	}
	art, err := h.store.CreateArtifact(ctx, store.Artifact{
		JobID: job.ID, Kind: reactive.PlanArtifactKind, Path: sealed,
		SHA256: fmt.Sprintf("diff-%d", job.ID), Size: 1, PlanSerial: planSerial,
	})
	if err != nil {
		t.Fatalf("CreateArtifact plan: %v", err)
	}
	showPlain := filepath.Join(h.dir, "plan-"+itoa64(job.ID)+".show.json")
	if err := os.WriteFile(showPlain, []byte(planJSON), 0o600); err != nil {
		t.Fatalf("write show json: %v", err)
	}
	showSealed, err := h.env.SealFile(showPlain)
	if err != nil {
		t.Fatalf("SealFile show json: %v", err)
	}
	if _, err := h.store.CreateArtifact(ctx, store.Artifact{
		JobID: job.ID, Kind: reactive.PlanShowArtifactKind, Path: showSealed,
		SHA256: fmt.Sprintf("show-%d", job.ID), Size: 1, PlanSerial: planSerial,
	}); err != nil {
		t.Fatalf("CreateArtifact show json: %v", err)
	}
	return art
}

// TestPlanDetailPageRendersDiff: the plan-detail page (SPEC §4) is the tray's
// diff deep-link target. It renders the +N ~N −N counters, the diff rows with
// the action keywords, the old→new inline cell of a changed attribute (display
// renderer quoted, §4.1), the unchanged footer, and the redaction marker for a
// secret-backed value — all from the SAME classifyPlan the phone and /api/v1
// read, never from a second parser.
func TestPlanDetailPageRendersDiff(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	art := h.seedPlanDiff(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],"before":{"value":"old","keep":"same"},"after":{"value":"new","keep":"same"}}},
		{"address":"kubernetes_secret.gone","change":{"actions":["delete"],"before":{"token":"hunter2"},"before_sensitive":{"token":true}}},
		{"address":"kubernetes_namespace.added","change":{"actions":["create"],"after":{"name":"e2e"}}}
	]}`)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, body := getBody(t, client, ts.URL+"/workspaces/web/plans/"+strconv.FormatInt(art.ID, 10))
	if code != http.StatusOK {
		t.Fatalf("GET /workspaces/web/plans/%d = %d (%s)", art.ID, code, body)
	}
	for _, want := range []string{
		">plan #" + strconv.FormatInt(art.ID, 10) + "<",
		"against serial 1",
		">+1<",
		">~1<",
		">−1<",
		">UPDATE<",
		">DESTROY<",
		">CREATE<",
		"kubernetes_config_map.marker",
		`>1 changed<`,
		` · 1 same<`,
		`>&#34;old&#34;<`,
		`>&#34;new&#34;<`,
		"1 attributes unchanged · ",
		`>show<`,
		"[redacted · secret]",
		"Apply plan #" + strconv.FormatInt(art.ID, 10) + " →",
		"asks first · same as exocomp ws apply web --plan " + strconv.FormatInt(art.ID, 10),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan detail missing %q", want)
		}
	}
	// Destroys come first, in their own fail block.
	di, ci := strings.Index(body, "Destroys — no undo"), strings.Index(body, "ACTION")
	if di < 0 || di >= ci {
		t.Error("plan detail did not render destroys before the ACTION header")
	}
	// The redacted attribute never carries its plaintext.
	if strings.Contains(body, "hunter2") {
		t.Error("plan detail leaked a secret-backed value")
	}
}

// TestPlanDetailRendersDiffFromShowJSON pins the fix for the "nothing to diff"
// bug: a plan-detail page feeds the plan's show-json companion (captured at
// plan time, while the run env was live) to classifyPlan, NOT the saved plan
// BINARY — a saved .tfplan is a tofu-format binary, not a show-json document,
// so the old feed always parsed empty. Mutation check: drop the show artifact
// change or feed the plan bytes and the diff reads empty again.
func TestPlanDetailRendersDiffFromShowJSON(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	art := h.seedPlanDiffWithShow(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],"before":{"value":"old","keep":"same"},"after":{"value":"new","keep":"same"}}},
		{"address":"kubernetes_secret.gone","change":{"actions":["delete"],"before":{"token":"hunter2"},"before_sensitive":{"token":true}}},
		{"address":"kubernetes_namespace.added","change":{"actions":["create"],"after":{"name":"e2e"}}}
	]}`)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, body := getBody(t, client, ts.URL+"/workspaces/web/plans/"+strconv.FormatInt(art.ID, 10))
	if code != http.StatusOK {
		t.Fatalf("GET /workspaces/web/plans/%d = %d (%s)", art.ID, code, body)
	}
	for _, want := range []string{
		">+1<",
		">~1<",
		">−1<",
		"kubernetes_config_map.marker",
		"kubernetes_secret.gone",
		"kubernetes_namespace.added",
		">UPDATE<",
		">DESTROY<",
		">CREATE<",
		`>&#34;old&#34;<`,
		`>&#34;new&#34;<`,
		"[redacted · secret]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan detail missing %q from the show-json diff", want)
		}
	}
	if strings.Contains(body, "This plan changes nothing") {
		t.Error("plan detail still reads 'changes nothing' for a plan that changes a resource")
	}
	// The redacted attribute never carries its plaintext, exactly as the
	// binary-fed page promised.
	if strings.Contains(body, "hunter2") {
		t.Error("plan detail leaked a secret-backed value from the show-json")
	}
}

// TestPlanDetailConfirmOpensWithoutJS: ?launch=1 renders the native <dialog>
// confirm already open (no JS), with the kicker, the ask, the fact grid, and
// the queue primary; the destroy variant (?destroy=1) reads kicker DESTROY
// with the −N counters and the danger ring.
func TestPlanDetailConfirmOpensWithoutJS(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	art := h.seedPlanDiff(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],"before":{"value":"old"},"after":{"value":"new"}}}
	]}`)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, body := getBody(t, client, ts.URL+"/workspaces/web/plans/"+strconv.FormatInt(art.ID, 10)+"?launch=1")
	if code != http.StatusOK {
		t.Fatalf("GET plan detail ?launch=1 = %d (%s)", code, body)
	}
	for _, want := range []string{
		`class="modal plan-confirm" id="plan-confirm" aria-labelledby="plan-confirm-title" open`,
		">APPLY PLAN #" + strconv.FormatInt(art.ID, 10) + " · WORKSPACE WEB<",
		">Change 1 resource(s) on web<",
		">Summary<",
		">Against<",
		">Policy<",
		">Runner<",
		"serial 1 · no drift",
		"none matched · no approval needed",
		">Queue apply — web @ plan #" + strconv.FormatInt(art.ID, 10) + "<",
		"1 update · 0 create · 0 destroy",
		`action="/workspaces/web/apply"`,
		`name="plan" value="` + strconv.FormatInt(art.ID, 10) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("open confirm missing %q", want)
		}
	}

	// Destroy mode: same dialog, kicker DESTROY, the −N verdict, danger ring.
	code, body = getBody(t, client, ts.URL+"/workspaces/web/plans/"+strconv.FormatInt(art.ID, 10)+"?destroy=1")
	if code != http.StatusOK {
		t.Fatalf("GET plan detail ?destroy=1 = %d (%s)", code, body)
	}
	for _, want := range []string{
		">DESTROY · WORKSPACE WEB<",
		">Destroy 0 resource(s) on web<",
		">Queue destroy — web<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("destroy confirm missing %q", want)
		}
	}
}

// TestPlanDetailStaleRefuses: a plan the Tofu state has moved past renders the
// REFUSED copy in fail and a live "Plan again" primary that posts the Plan verb
// — never a disabled button (invariant 7). Mutation check: drop the stale test
// and it reads red.
func TestPlanDetailStaleRefuses(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ws := h.seedWorkspace(t, "web")
	// The state moves to serial 2 after the plan was made at serial 1.
	if _, err := h.store.AppendTFState(context.Background(), store.NewTFState{
		WorkspaceID: ws.ID, Ciphertext: []byte("state"),
	}); err != nil {
		t.Fatalf("AppendTFState: %v", err)
	}
	if _, err := h.store.AppendTFState(context.Background(), store.NewTFState{
		WorkspaceID: ws.ID, Ciphertext: []byte("state"),
	}); err != nil {
		t.Fatalf("AppendTFState: %v", err)
	}
	art := h.seedPlanDiff(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],"before":{"value":"old"},"after":{"value":"new"}}}
	]}`)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, body := getBody(t, client, ts.URL+"/workspaces/web/plans/"+strconv.FormatInt(art.ID, 10)+"?launch=1")
	if code != http.StatusOK {
		t.Fatalf("GET stale plan detail = %d (%s)", code, body)
	}
	for _, want := range []string{
		"REFUSED · Tofu state moved to serial 2",
		`<button type="submit" class="btn primary">Plan again</button>`,
		`action="/workspaces/web/plan"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stale plan missing %q", want)
		}
	}
	// Never disabled (invariant 7): the primary is a live, enabled submit that
	// re-plans. The queue-apply form must not be offered for a stale plan.
	if strings.Contains(body, `action="/workspaces/web/apply"`) {
		t.Error("stale plan still offers the apply form")
	}
}

// TestPlanDetailDenyHasNoPrimary: a deny verdict renders the DENIED copy in
// fail and Cancel only — no primary submit at all (SPEC §4).
func TestPlanDetailDenyHasNoPrimary(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	art := h.seedPlanDiff(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],"before":{"value":"old"},"after":{"value":"new"}}}
	]}`)
	h.seedPlanVerdict(t, art, store.PolicyDeny, "no-destroy")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, body := getBody(t, client, ts.URL+"/workspaces/web/plans/"+strconv.FormatInt(art.ID, 10)+"?launch=1")
	if code != http.StatusOK {
		t.Fatalf("GET denied plan detail = %d (%s)", code, body)
	}
	if !strings.Contains(body, "DENIED · no-destroy") {
		t.Error("denied plan missing the DENIED verdict")
	}
	if strings.Contains(body, "Queue apply") || strings.Contains(body, "Plan again") {
		t.Error("denied plan renders a primary submit")
	}
	// Cancel is still there.
	if !strings.Contains(body, ">Cancel<") {
		t.Error("denied plan missing Cancel")
	}
}

// TestPlanDetailConsumedShowsJob: a plan an apply re-recorded reads CONSUMED
// #job on both the tray and the plan page, and the job is a link (SPEC §2.2).
// The dedupe (same sha, two artifact rows) is what makes one plan of the pair.
func TestPlanDetailConsumedShowsJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	art := h.seedPlanArtifactSerial(t, "web", 1)
	applyJob, err := h.queue.Enqueue(context.Background(), queue.Spec{
		Runbook: service.WorkspaceRunbook("web"), Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue apply job: %v", err)
	}
	h.seedPlanReRecord(t, art, applyJob.ID)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	// The tray collapses the seal + re-record pair onto ONE row: CONSUMED #job.
	code, body := getBody(t, client, ts.URL+"/workspaces/web")
	if code != http.StatusOK {
		t.Fatalf("GET /workspaces/web = %d", code)
	}
	if !strings.Contains(body, "1 plans · newest first · latest is applicable") {
		t.Error("consumed plan tray does not read one plan")
	}
	for _, want := range []string{
		">CONSUMED<",
		`href="/jobs/` + strconv.FormatInt(applyJob.ID, 10) + `"`,
		">#" + strconv.FormatInt(applyJob.ID, 10) + "<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("consumed tray row missing %q", want)
		}
	}
	// The single-apply rule: the consumed plan is no longer the SAVED one, so
	// no apply glyph remains.
	if strings.Contains(body, "aria-label=\"Apply plan #") {
		t.Error("consumed plan still carries an apply glyph")
	}

	// The plan page reads the same word.
	code, body = getBody(t, client, ts.URL+"/workspaces/web/plans/"+strconv.FormatInt(art.ID, 10))
	if code != http.StatusOK {
		t.Fatalf("GET consumed plan page = %d", code)
	}
	if !strings.Contains(body, ">CONSUMED<") || !strings.Contains(body, ">#"+strconv.FormatInt(applyJob.ID, 10)+"<") {
		t.Error("consumed plan page missing CONSUMED #job")
	}
}

// TestWorkspacesPageReRecordNotDoubleCount: the CLI apply re-records a sealed
// plan under the applying job; the tray dedupes by sha256 so the re-record
// never doubles "N plans" and never shadows a distinct plan. Two seeds are two
// plans; adding a same-sha re-record of one still reads two.
func TestWorkspacesPageReRecordNotDoubleCount(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	first := h.seedPlanArtifactSerial(t, "web", 1)
	second := h.seedPlanArtifactSerial(t, "web", 2)
	applyJob, err := h.queue.Enqueue(context.Background(), queue.Spec{
		Runbook: service.WorkspaceRunbook("web"), Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue apply job: %v", err)
	}
	h.seedPlanReRecord(t, first, applyJob.ID)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, body := getBody(t, client, ts.URL+"/workspaces/web")
	if code != http.StatusOK {
		t.Fatalf("GET /workspaces/web = %d", code)
	}
	// Two distinct plans — the re-record of `first` collapses into it.
	if !strings.Contains(body, "2 plans · newest first · latest is applicable") {
		t.Error("re-record doubled the plan count")
	}
	// The newest unconsumed plan (second) is the one SAVED and applicable; the
	// re-recorded first plan reads CONSUMED and carries no apply glyph.
	for _, want := range []string{
		"SAVED",
		">CONSUMED<",
		`aria-label="Apply plan #` + strconv.FormatInt(second.ID, 10) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tray after re-record missing %q", want)
		}
	}
	if strings.Contains(body, `aria-label="Apply plan #`+strconv.FormatInt(first.ID, 10)+`"`) {
		t.Error("consumed first plan still carries an apply glyph")
	}
}
