package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// seedPostV1 registers one of each post-v1 object the catch-up surfaces show: a
// signed git source, a drift-check schedule whose last fire succeeded, an
// in-flight provision->converge workflow, and a require_approval plan policy.
func seedPostV1(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()

	if _, err := h.store.CreateSource(ctx, store.Source{
		Name: "infra", Kind: store.SourceGit, URI: "git@host:infra.git",
		Ref: "refs/heads/main", LastSyncSHA: "0123456789abcdef0123",
		Signed: true, AllowedSigners: "ssh-ed25519 AAAA... op@host", Enabled: true,
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	// A drift-check schedule with a succeeded last fire: the last-outcome chip is
	// its last drift result.
	sc, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "drift-check", CronExpr: "*/15 * * * *", Kind: "tofu-plan",
	})
	if err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	// Enqueued and run through the live queue, same as the daemon would, so
	// nothing here hand-drives a state the harness's own q.Run(claimAny=true)
	// is concurrently claiming.
	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "plan-prod", Kind: "tofu", Trigger: "schedule",
		Argv: []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("seed schedule job: %v", err)
	}
	job = h.waitJobState(t, job.ID, store.StateSucceeded, 2*time.Second)
	next := time.Now().Add(15 * time.Minute).UTC()
	if err := h.store.RecordScheduleFire(ctx, sc.ID, &job.ID, &next); err != nil {
		t.Fatalf("record fire: %v", err)
	}

	// An in-flight workflow: two steps, the first running, so it is incomplete
	// and shows on the dashboard.
	const wfID = "provision-web-01"
	if err := h.store.CreateWorkflow(ctx, wfID, []store.NewWorkflowStep{
		{StepNo: 0, Kind: "tofu-apply", IdempotencyKey: "k0"},
		{StepNo: 1, Kind: "ansible-converge", IdempotencyKey: "k1"},
	}); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if err := h.store.StartStep(ctx, wfID, 0, job.ID); err != nil {
		t.Fatalf("start step: %v", err)
	}

	if _, err := h.store.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "no-destroy-prod", Workspace: "prod",
		Expr:   "plan.resource_changes.exists(c, c.change.actions == ['delete'])",
		Action: store.PolicyRequireApproval,
	}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
}

// TestDashboardPostV1Sections is the GATE: the dashboard's post-v1 cards
// (sources, schedules, workflows) summarise the seeded objects as counts
// rather than embedding their rows — the source name, its trust chip, the
// schedule name/cron, and the workflow's id all stay off the glance screen,
// reachable only by following the card's link to its full view. Control-plane
// health moved to /system (asserted separately) and no longer renders here.
func TestDashboardPostV1Sections(t *testing.T) {
	h := newHarness(t)
	seedPostV1(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != http.StatusOK {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		// card chrome + a11y
		`aria-labelledby="sources-h"`,
		`aria-labelledby="sched-h"`,
		`aria-labelledby="flows-h"`,
		`<a class="card cardlink" href="/schedules" aria-labelledby="sched-h">`,
		// the sources card's count, not a per-row list
		`<h3 id="sources-h">Sources</h3>`,
		`<div class="cardnum">1</div>`,
		`<p class="cardlabel">sources</p>`,
		// the workflows card's count, not a per-row list
		`<h3 id="flows-h">Workflows in flight</h3>`,
		`<p class="cardlabel">in flight</p>`,
		// the schedules card summarises the seeded row as a count, not a list
		`1 schedules · 0 disabled`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"infra",                       // the source's name — only its count shows now
		"SIGNED · SIGNERS",            // the source's per-row trust chip
		"drift-check",                 // the schedule's name — only its count shows now
		"*/15 * * * *",                // the schedule's cron — a /schedules concern
		"provision-web-01",            // the workflow's id — only its count shows now
		`aria-label="workflow steps"`, // the workflow's per-row step rail
	} {
		if strings.Contains(page, unwanted) {
			t.Errorf("dashboard should summarise, not list — found %q", unwanted)
		}
	}
	// The control-plane health card left the dashboard for /system.
	if strings.Contains(page, `aria-labelledby="self-h"`) {
		t.Error("control-plane health card should have left the dashboard for /system")
	}
}

// TestSystemPageHealth is the GATE for /system: the control-plane health card
// (running version + Litestream RPO) and the backups card, moved off the
// dashboard and the powerline, render on the shipped table chrome.
func TestSystemPageHealth(t *testing.T) {
	h := newHarness(t)
	seedPostV1(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("system status = %d", code)
	}
	for _, want := range []string{
		`aria-labelledby="self-h"`,
		`aria-labelledby="backups-h"`,
		`scope="col"`,
		"Running version",
		"Litestream RPO",
		"no replica configured",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("system page missing %q", want)
		}
	}
}

// TestPostV1RoutesGated: each new route refuses a request with no session
// (redirect to login), and renders for a session (ADR 0009, the gate_test
// pattern).
func TestPostV1RoutesGated(t *testing.T) {
	h := newHarness(t)
	seedPostV1(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	routes := []struct {
		path string
		want string
	}{
		{"/sources", "Sources"},
		{"/schedules", "Schedules"},
		{"/workflows", "Workflows"},
		{"/workflows/provision-web-01", "Step ledger"},
		{"/policies", "Plan policies"},
	}

	// No session: refused with a redirect to the login form.
	noSession := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	for _, rt := range routes {
		resp, err := noSession.Get(ts.URL + rt.path)
		if err != nil {
			t.Fatalf("GET %s: %v", rt.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("%s without session = %d, want 303", rt.path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Errorf("%s redirect = %q, want /login…", rt.path, loc)
		}
	}

	// With a session: each renders its heading and carries table a11y scope.
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	for _, rt := range routes {
		code, page := getBody(t, client, ts.URL+rt.path)
		if code != http.StatusOK {
			t.Errorf("%s with session = %d, want 200", rt.path, code)
			continue
		}
		if !strings.Contains(page, rt.want) {
			t.Errorf("%s missing %q", rt.path, rt.want)
		}
		if !strings.Contains(page, `scope="col"`) {
			t.Errorf("%s missing table scope a11y", rt.path)
		}
	}
}

// TestWorkflowDetailLedger: the detail view is the ordered step ledger with each
// step's status read from the rows, and a 404 shape for an unknown id.
func TestWorkflowDetailLedger(t *testing.T) {
	h := newHarness(t)
	seedPostV1(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/workflows/provision-web-01")
	if code != http.StatusOK {
		t.Fatalf("workflow detail status = %d", code)
	}
	for _, want := range []string{"tofu-apply", "ansible-converge", "running", "pending", "Step ledger"} {
		if !strings.Contains(page, want) {
			t.Errorf("workflow detail missing %q", want)
		}
	}

	code, page = getBody(t, client, ts.URL+"/workflows/does-not-exist")
	if code != http.StatusNotFound {
		t.Errorf("unknown workflow status = %d, want 404", code)
	}
	if !strings.Contains(page, "No workflow with this id") {
		t.Error("unknown workflow should render the not-found notice")
	}
}

// TestSchedulesDriftOutcome: the schedules surface shows a drift-check
// schedule's last outcome as a state chip linking to its job.
func TestSchedulesDriftOutcome(t *testing.T) {
	h := newHarness(t)
	seedPostV1(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/schedules")
	if code != http.StatusOK {
		t.Fatalf("schedules status = %d", code)
	}
	for _, want := range []string{"drift-check", "tofu-plan", "succeeded", `class="chip ok"`} {
		if !strings.Contains(page, want) {
			t.Errorf("schedules missing %q", want)
		}
	}
}

// TestPoliciesVerdict: a require_approval policy renders its verbatim expression
// and a waiting-on-human verdict chip.
func TestPoliciesVerdict(t *testing.T) {
	h := newHarness(t)
	seedPostV1(t, h)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/policies")
	if code != http.StatusOK {
		t.Fatalf("policies status = %d", code)
	}
	for _, want := range []string{"no-destroy-prod", "require_approval", "resource_changes"} {
		if !strings.Contains(page, want) {
			t.Errorf("policies missing %q", want)
		}
	}
}
