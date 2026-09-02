package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// seedRules registers a shell rule with a source filter and guardrails, a
// playbook rule with a required approval, and a bare notify rule with no
// filter, so the page renders every action shape and both guardrail edges
// (the `rule list` labels included).
func seedRules(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()

	if _, err := h.store.CreateRule(ctx, store.NewRule{
		Name: "restart-nginx", SourceFilter: "alertmanager",
		CELExpr:    `event.labels.severity == "critical"`,
		ActionKind: store.ActionShell,
		Action:     store.RuleAction{Argv: []string{"systemctl", "restart", "nginx"}},
		CooldownS:  1800, DailyCap: 5,
	}); err != nil {
		t.Fatalf("seed shell rule: %v", err)
	}
	if _, err := h.store.CreateRule(ctx, store.NewRule{
		Name:         "converge",
		SourceFilter: "check",
		CELExpr:      `event.labels.netgroup == "home"`,
		ActionKind:   store.ActionPlaybook,
		Action: store.RuleAction{
			Source: "infra", Playbook: "site.yml", Limit: "laptops",
		},
		RequireApproval: true,
	}); err != nil {
		t.Fatalf("seed playbook rule: %v", err)
	}
	if _, err := h.store.CreateRule(ctx, store.NewRule{
		Name: "page-me", CELExpr: "true", ActionKind: store.ActionNotify,
	}); err != nil {
		t.Fatalf("seed notify rule: %v", err)
	}
}

// TestRulesSurfaceGated is the gate for the rules view: token-gated (no
// session refused, session renders), shows every rule with the action and
// guardrails `rule list` renders, and — rules stay CLI-managed — exposes no
// write control to any session, operator included.
func TestRulesSurfaceGated(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedRules(t, h)

	// No session: refused with a redirect to the login form.
	noSession := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noSession.Get(ts.URL + "/rules")
	if err != nil {
		t.Fatalf("GET /rules no session: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/rules without session = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("/rules redirect = %q, want /login…", loc)
	}

	// Operator: the surface renders with every rule, its action, and the
	// guardrails — and no write control of any kind.
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, op, ts.URL+"/rules")
	if code != http.StatusOK {
		t.Fatalf("/rules with session = %d, want 200", code)
	}
	for _, want := range []string{
		`scope="col"`, "All rules",
		"restart-nginx", "converge", "page-me",
		`<th scope="col">Rule</th>`, `<th scope="col">Enabled</th>`,
		`<th scope="col">Source</th>`, `<th scope="col">Action</th>`,
		`<th scope="col">Cooldown</th>`, `<th scope="col">Cap</th>`,
		`<th scope="col">Approval</th>`, `<th scope="col">Expression</th>`,
		"alertmanager", "check", "any",
		"shell systemctl restart nginx",
		"playbook infra site.yml (limit laptops)",
		"notify",
		"30m0s", "5", "-", "required",
		`event.labels.severity == &#34;critical&#34;`,
		`event.labels.netgroup == &#34;home&#34;`,
		`href="/rules" aria-current="page"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/rules (operator) missing %q", want)
		}
	}
	if strings.Contains(page, "<form") || strings.Contains(page, `method="post"`) {
		t.Error("/rules (operator) must expose no mutation form")
	}
	// The default ENABLED chip is hidden (F12) — the column header carries the
	// default; only a divergent DISABLED would chip.
	if strings.Contains(page, `<span class="chip ok"><span class="dot"></span>ENABLED</span>`) {
		t.Error("/rules still renders the default ENABLED chip (F12)")
	}

	// Readonly: renders identically, still no write control.
	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))
	code, page = getBody(t, ro, ts.URL+"/rules")
	if code != http.StatusOK {
		t.Fatalf("/rules readonly = %d, want 200", code)
	}
	if !strings.Contains(page, "Read-only: rules stay CLI-managed") {
		t.Error("/rules (readonly) missing the read-only note")
	}
	if strings.Contains(page, "<form") || strings.Contains(page, `method="post"`) {
		t.Error("/rules (readonly) must expose no mutation form")
	}
}

// TestRulesNavElsewhere: the Rules nav item appears on other pages too, so it
// is reachable from anywhere in the surface.
func TestRulesNavElsewhere(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, page := getBody(t, client, ts.URL+"/")
	if !strings.Contains(page, `href="/rules"`) || !strings.Contains(page, ">Rules</span>") {
		t.Error("dashboard nav missing the Rules link")
	}
}
