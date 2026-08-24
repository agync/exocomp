package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestPolicyAddValidatesExpr(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/policies/add",
		url.Values{
			"name": {"bad"}, "workspace": {"prod"},
			"action": {store.PolicyDeny}, "expr": {"this is not valid CEL ((("},
		}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("an uncompilable expression was accepted")
	}
	if _, err := h.store.GetPlanPolicy(context.Background(), "bad"); err == nil {
		t.Fatal("a policy was created despite an uncompilable expression")
	}
}

func TestPolicyAddValidatesAction(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/policies/add",
		url.Values{
			"name": {"bad-action"}, "workspace": {"prod"},
			"action": {"nonsense"}, "expr": {"true"},
		}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("an unknown action was accepted")
	}
	if _, err := h.store.GetPlanPolicy(context.Background(), "bad-action"); err == nil {
		t.Fatal("a policy was created despite an unknown action")
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
