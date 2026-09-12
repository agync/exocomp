package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

func TestPolicyWriteReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/policies/add",
		url.Values{"name": {"no-destroy"}, "workspace": {"prod"}, "action": {store.PolicyDeny}, "expr": {"true"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly policy add = %d, want 403", resp.StatusCode)
	}
}

func TestPolicyWriteCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/policies/add",
		url.Values{"name": {"no-destroy"}, "workspace": {"prod"}, "action": {store.PolicyDeny}, "expr": {"true"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin policy add = %d, want 403", resp.StatusCode)
	}
}

func TestPolicyAddCreates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	resp := postForm(t, client, ts.URL+"/policies/add",
		url.Values{
			"name": {"no-destroy"}, "workspace": {"prod"},
			"action": {store.PolicyDeny}, "expr": {"true"},
		}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("policy add = %d, want 303", resp.StatusCode)
	}

	rows, err := h.store.ListPlanPolicies(ctx, "", false)
	if err != nil {
		t.Fatalf("ListPlanPolicies: %v", err)
	}
	var found bool
	for _, p := range rows {
		if p.Name == "no-destroy" {
			found = true
			if p.Workspace != "prod" {
				t.Fatalf("workspace = %q, want prod", p.Workspace)
			}
			if p.Action != store.PolicyDeny {
				t.Fatalf("action = %q, want %q", p.Action, store.PolicyDeny)
			}
			if !p.Enabled {
				t.Fatal("newly created policy should be enabled")
			}
		}
	}
	if !found {
		t.Fatal("policy was not created")
	}
}

// TestPolicyFormAtRest renders the policies-add dialog for an operator and
// asserts the forms-round anatomy: the reusable field pattern with
// always-present meta lines (help at rest), the action select's own meta
// line, aria wiring tying each control to its meta, a form-status line, and
// novalidate so the daemon stays authoritative.
func TestPolicyFormAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/policies")
	if code != http.StatusOK {
		t.Fatalf("/policies = %d, want 200", code)
	}
	for _, want := range []string{
		`id="policies-add"`,
		`action="/policies/add"`,
		`id="policy-add-form" novalidate`,
		`aria-describedby="pl-name-meta"`,
		`aria-describedby="pl-workspace-meta"`,
		`aria-describedby="pl-action-meta"`,
		`aria-describedby="pl-expr-meta"`,
		`id="pl-name-meta"`,
		`id="pl-workspace-meta"`,
		`id="pl-action-meta"`,
		`id="pl-expr-meta"`,
		"Letters, digits, dots, hyphens, underscores.",
		"The workspace this gate covers.",
		"On match is deny or require_approval.",
		"A CEL expression over plan.",
		`role="status" aria-live="polite"`,
		"Add policy",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("policy form missing %q", want)
		}
	}
	// At rest no control carries a refusal and the metas are plain help text.
	if !strings.Contains(page, `placeholder="no-destroy" aria-describedby="pl-name-meta">`) {
		t.Error("at-rest name control carries an aria-invalid refusal")
	}
	if !strings.Contains(page, `id="pl-name-meta" class="meta">Letters`) {
		t.Error("at-rest name meta is not plain help text")
	}
}

// TestPolicyFormRefusalsRender asserts the 422 re-render fills every meta line
// at once (name, workspace, action, expr), keeps the typed values, and never
// flashes: a refusal is the form's own surface, flash is success-only.
func TestPolicyFormRefusalsRender(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/policies/add",
		url.Values{
			"name": {"bad name!"}, "workspace": {""},
			"action": {"nonsense"}, "expr": {"this is not valid CEL ((("},
		}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("refusal status = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{
		`aria-invalid="true"`,
		"Names can&#39;t have spaces.",
		"Give the workspace a name.",
		"On match is deny or require_approval.",
		"The expression can&#39;t compile — check the CEL syntax.",
		`value="bad name!"`,
		`exoModalOpen("policies-add")`,
		`autofocus`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("422 re-render missing %q", want)
		}
	}
	// A refusal never flashes: the render consumes the flash cookie (an empty
	// clearing Set-Cookie), so no flash MESSAGE may ride this response.
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("policy refusal carried a flash message")
		}
	}
}

func TestPolicyAddValidatesExpr(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/policies/add",
		url.Values{
			"name": {"bad"}, "workspace": {"prod"},
			"action": {store.PolicyDeny}, "expr": {"this is not valid CEL ((("},
		}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expr refusal status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "The expression can&#39;t compile") {
		t.Errorf("expr refusal missing the expr-field sentence")
	}
	if _, err := h.store.GetPlanPolicy(context.Background(), "bad"); err == nil {
		t.Fatal("a policy was created despite an uncompilable expression")
	}
}

func TestPolicyAddValidatesAction(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/policies/add",
		url.Values{
			"name": {"bad-action"}, "workspace": {"prod"},
			"action": {"nonsense"}, "expr": {"true"},
		}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("action refusal status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "On match is deny or require_approval.") {
		t.Errorf("action refusal missing the action-select sentence")
	}
	if _, err := h.store.GetPlanPolicy(context.Background(), "bad-action"); err == nil {
		t.Fatal("a policy was created despite an unknown action")
	}
}

// TestPolicyRmRefusalRendersOnSurface: removing a policy that is already gone
// re-renders the surface at 404 with the refusal shown on it — never a flash
// (flash is success-only) — and nothing crashes.
func TestPolicyRmRefusalRendersOnSurface(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/policies/nope/rm", url.Values{"confirmed": {"1"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rm refusal status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body, "No policy named nope exists to remove.") {
		t.Errorf("rm refusal missing the on-surface sentence")
	}
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("rm refusal carried a flash message")
		}
	}
}

func TestPolicyRm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "to-remove", Workspace: "prod", Expr: "true", Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("CreatePlanPolicy: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/policies/to-remove/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm = %d, want 303", resp.StatusCode)
	}
	if _, err := h.store.GetPlanPolicy(ctx, "to-remove"); err == nil {
		t.Fatal("policy still present after rm")
	}
}
