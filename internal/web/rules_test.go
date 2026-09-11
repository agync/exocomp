package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// seedRules registers a shell rule with a source filter and guardrails, a
// playbook rule with a required approval, and a bare notify rule with no
// filter, so the page renders every action shape and both guardrail edges
// (the two-line row's guards, SPEC §5.3). It also fires the playbook rule
// (job #1 · exit 0) and the notify rule (no job) into the rule_fire ledger, so
// LAST FIRED reads both outcomes; the shell rule stays never-fired.
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
			Egress: []int{22, 443},
		},
		RequireApproval: true,
	}); err != nil {
		t.Fatalf("seed playbook rule: %v", err)
	}
	pageMe, err := h.store.CreateRule(ctx, store.NewRule{
		Name: "page-me", CELExpr: "true", ActionKind: store.ActionNotify,
	})
	if err != nil {
		t.Fatalf("seed notify rule: %v", err)
	}

	// The playbook rule's fire names a real, settled job (exit 0).
	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "playbook", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue fire job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("start fire job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("settle fire job: %v", err)
	}
	if err := h.store.SetJobExitCode(ctx, job.ID, 0); err != nil {
		t.Fatalf("set fire job exit: %v", err)
	}
	converge, err := h.store.GetRule(ctx, "converge")
	if err != nil {
		t.Fatalf("get playbook rule: %v", err)
	}
	if _, err := h.store.RecordRuleFire(ctx, converge.ID, &job.ID, ""); err != nil {
		t.Fatalf("record playbook fire: %v", err)
	}
	// The notify rule matched but produced no job.
	if _, err := h.store.RecordRuleFire(ctx, pageMe.ID, nil, ""); err != nil {
		t.Fatalf("record notify fire: %v", err)
	}
}

// TestRulesSurfaceGated is the gate for the rules view: token-gated (no
// session refused, session renders), shows every rule as a two-line row (SPEC
// §5.3) — line one the CEL expression (one line, ellipsis), line two `then` +
// action + the 20px guard words (egress / cooldown / gate in wait-tint) — and
// LAST FIRED from the rule_fire ledger. Rules stay CLI-managed: no write
// control to any session, operator included.
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

	// Operator: the surface renders every rule, its action phrase, the guards,
	// the ledger's LAST FIRED — and no write control of any kind.
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, op, ts.URL+"/rules")
	if code != http.StatusOK {
		t.Fatalf("/rules with session = %d, want 200", code)
	}
	converge, err := h.store.GetRule(context.Background(), "converge")
	if err != nil {
		t.Fatalf("GetRule converge: %v", err)
	}
	fire, err := h.store.LastRuleFire(context.Background(), converge.ID)
	if err != nil {
		t.Fatalf("LastRuleFire: %v", err)
	}
	for _, want := range []string{
		"All rules",
		"restart-nginx", "converge", "page-me",
		"RULE", "WHEN · THEN", "LAST FIRED",
		`class="th-actions"`,
		"shell systemctl restart nginx",
		"playbook infra/site.yml",
		"log only · no action, no guards",
		"cooldown 30m",
		"egress 22, 443",
		"gate approval · TTL 30m",
		`class="guard guard--gate">gate approval · TTL 30m`,
		`event.labels.severity == &#34;critical&#34;`,
		`event.labels.netgroup == &#34;home&#34;`,
		`aria-label="Explain last evaluation"`,
		fire.Ts.Local().Format("15:04:05"),
		"job #" + itoa64(jobOf(t, h)) + " · exit 0",
		"matched · no job",
		`href="/rules" aria-current="page"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/rules (operator) missing %q", want)
		}
	}
	// The ENABLED/DISABLED keyword names every rule's posture (SPEC §5.3) —
	// the new two-line row supersedes the F12 column-hiding for rules.
	for _, want := range []string{
		`<span class="chip ok"><span class="dot"></span>ENABLED</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/rules missing %q", want)
		}
	}
	if strings.Contains(page, "<form") || strings.Contains(page, `method="post"`) {
		t.Error("/rules (operator) must expose no mutation form")
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

// TestRulesNoOrphanSeparator is the QA delta's rules mutation: the `then`
// line draws a separator ONLY between two rendered verbs — a rule with guards
// reads `then <action> · [guard …]`, a no-guards rule reads `then <action>`
// with NO trailing separator — and the ACTIONS slot renders exactly the
// explain · ▾ pair per rule (SCREENS 1d), never a stray marker or an orphan
// separator alone. The native <details> disclosure triangle must stay
// suppressed (list-style:none + the webkit marker), so it never reads as a
// third stray character.
func TestRulesNoOrphanSeparator(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedRules(t, h)

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, client, ts.URL+"/rules")
	if code != http.StatusOK {
		t.Fatalf("/rules = %d, want 200", code)
	}
	// Exactly the guarded rules draw the separator: restart-nginx (cooldown)
	// and converge (egress · gate) = 2; the notify rule never does.
	if n := strings.Count(page, `<span class="sep">·</span>`); n != 2 {
		t.Errorf("rules page draws %d separators, want 2 (one per guarded rule)", n)
	}
	// The no-guards rule reads `then log only · no action, no guards` with the
	// action span closing the div — no trailing separator.
	if !strings.Contains(page, "log only · no action, no guards</span> </div>") {
		t.Error("a no-guards rule must render then <action> with NO trailing separator")
	}
	// ACTIONS = explain · ▾ (SCREENS 1d): one chevron per rule, no extra.
	if n := strings.Count(page, `d="M6 9l6 6 6-6"`); n != 3 {
		t.Errorf("rules ACTIONS renders %d chevrons, want one per rule (explain · ▾)", n)
	}
	if n := strings.Count(page, `aria-label="Explain last evaluation"`); n != 3 {
		t.Errorf("rules ACTIONS renders %d explain verbs, want one per rule", n)
	}
	// The native disclosure triangle is suppressed, not left as a stray glyph.
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	if !strings.Contains(app, ".rule-explain summary::-webkit-details-marker { display: none; }") {
		t.Error("app.css must suppress the native details marker (webkit)")
	}
	summary := ruleDecl(app, ".rule-explain summary {")
	if !strings.Contains(summary, "list-style: none") {
		t.Error(".rule-explain summary must carry list-style:none")
	}
}

// jobOf returns the seeded fire job's id — the first (only) job on the row.
func jobOf(t *testing.T, h *harness) int64 {
	t.Helper()
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) == 0 {
		t.Fatal("no seeded fire job")
	}
	return jobs[0].ID
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
