package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// TestPowerlineSignalSegments is the GATE for the F10 realign: each powerline
// segment is dot + word in its signal color and links to where it is fixed, so
// a gap reads as a verb, not a grey fact. A fresh harness has no backup (dead-man
// none → wait + "— set one"), no push transport (ntfy off → deliberate dim),
// and no data dir (disk —, no state class).
func TestPowerlineSignalSegments(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`class="pl-seg wait"`,
		`dead-man none`,
		`— set one`,
		`class="pl-seg dim"`,
		`ntfy off`,
		`href="/system"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("powerline missing %q", want)
		}
	}
	for _, gone := range []string{
		`pl-item`,
		`pl-item warn`,
	} {
		if strings.Contains(page, gone) {
			t.Errorf("powerline still carries the old grey-fact markup %q", gone)
		}
	}
}

// TestDashboardSummaryCards is the GATE for the at-a-glance redesign: the
// dashboard shows summary cards — counts plus a single attention detail —
// never an embedded table, and settled job history never reaches the page at
// all (only the active queue is counted). A pending approval is the primary
// attention item and carries the wait/amber accent with a one-line subline
// naming the soonest-to-expire item; a recent job failure surfaces the same
// way on the queue card.
func TestDashboardSummaryCards(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Running: a long-lived job, waited into StateRunning so the count is
	// read deterministically rather than racing the live queue workers.
	running, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "running-job", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/sleep", "30"},
	})
	if err != nil {
		t.Fatalf("enqueue running job: %v", err)
	}
	h.waitJobState(t, running.ID, store.StateRunning, 3*time.Second)

	// Pending approval: also the queue's one non-running active job — an
	// awaiting_approval row is still part of the active queue.
	_, approval := h.gatedJob(t, 30*time.Minute)

	// Settled history: must never appear on the dashboard at all.
	settled, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "settled-history-job", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("enqueue settled job: %v", err)
	}
	h.waitJobState(t, settled.ID, store.StateSucceeded, 3*time.Second)

	// A recent failure: the queue card's fail-colored subline.
	failed, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "recently-failed-job", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/sh", "-c", "exit 1"},
	})
	if err != nil {
		t.Fatalf("enqueue failing job: %v", err)
	}
	h.waitJobState(t, failed.ID, store.StateFailed, 3*time.Second)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}

	// No table survives on the dashboard: nothing was seeded for the
	// Schedules card (a summary card since the glance redesign, like every
	// other card), so the absence of any table here proves every card renders
	// as a summary, not a list.
	if strings.Contains(page, "<table") {
		t.Error("dashboard still renders a <table>; summary cards should replace embedded lists")
	}
	// The settled job's own runbook must never reach the page — only its
	// exclusion from the active count does.
	if strings.Contains(page, "settled-history-job") {
		t.Error("dashboard shows settled job history; the queue card must count active jobs only")
	}

	// Pending approvals: the primary attention item, wait/amber accent, one
	// subline naming the soonest item (the only one here) and its deadline.
	for _, want := range []string{
		`<a class="card cardlink attn" href="/approvals" aria-labelledby="needsyou-h">`,
		`<div class="cardnum warn">1</div>`,
		`<p class="cardlabel">waiting on you</p>`,
		`class="cardsub warn"`,
		"deploy-app", // gatedJob's fixed runbook — the soonest (only) item's target
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing approvals attention marker %q", want)
		}
	}
	_ = approval // the ID isn't asserted; only its effect on the card is

	// Queue: active-only counts, plus the recent-failure subline in the fail
	// color.
	for _, want := range []string{
		`<a class="card cardlink" href="/jobs" aria-labelledby="queue-h">`,
		`<div class="cardnum">1 running · 1 queued</div>`,
		`<p class="cardsub fail">1 failed recently</p>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing queue marker %q", want)
		}
	}

	// Workspaces and Sources: nothing registered, so both stay calm and link
	// through.
	for _, want := range []string{
		`<a class="card cardlink" href="/workspaces" aria-labelledby="ws-h">`,
		`<p class="cardsub meta">No workspaces registered.</p>`,
		`<a class="card cardlink" href="/sources" aria-labelledby="sources-h">`,
		`<p class="cardsub meta">No sources registered.</p>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing calm-state marker %q", want)
		}
	}

	// Workflows: nothing in flight — the card is noise when it's usually
	// empty, so it is hidden entirely rather than shown as a permanent zero.
	// (The sidebar nav always links to /workflows; only the card's own
	// aria-labelledby marks whether the card itself rendered.)
	if strings.Contains(page, `aria-labelledby="flows-h"`) {
		t.Error("workflows card should be hidden when nothing is in flight")
	}
}

// TestDashboardApprovalsCardCalm asserts the pending-approvals card's calm
// state: with nothing pending, the card carries no attention accent and shows
// the "None waiting." line, not a bare zero.
func TestDashboardApprovalsCardCalm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	if !strings.Contains(page, `<a class="card cardlink" href="/approvals" aria-labelledby="needsyou-h">`) {
		t.Error("approvals card should render without the attn accent when nothing is pending")
	}
	if !strings.Contains(page, `<p class="cardsub meta">None waiting.</p>`) {
		t.Error(`approvals card should show "None waiting." when nothing is pending`)
	}
	if strings.Contains(page, "cardlink attn") {
		t.Error("no card should carry the attention accent on an otherwise-empty estate")
	}
	if !strings.Contains(page, `<p class="cardsub meta">Idle.</p>`) {
		t.Error(`queue card should show "Idle." when nothing is active`)
	}
}

// TestDashboardChecksCard asserts the probe-estate card: the registered total,
// the "N targets up" subline from the aggregate check_state count, and the
// attention accent exactly when a check is disabled — DOWN is normal for an
// away host, so only a disabled check flags the card.
func TestDashboardChecksCard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	enabled, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	now := time.Now().UTC()
	if err := h.store.SetCheckState(ctx, store.CheckState{
		CheckID: enabled.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 1, Emitted: store.CheckUp, FirstSeen: now, LastChange: now,
	}); err != nil {
		t.Fatalf("SetCheckState: %v", err)
	}
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "roaming", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "phone", Address: "192.168.1.70", Port: 22}},
	}); err != nil {
		t.Fatalf("CreateCheck disabled fixture: %v", err)
	}
	if err := h.store.SetCheckEnabled(ctx, "roaming", false); err != nil {
		t.Fatalf("disable check: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`<a class="card cardlink attn" href="/checks" aria-labelledby="checks-h">`,
		`<div class="cardnum">2</div>`,
		`<p class="cardlabel">checks</p>`,
		`<p class="cardsub warn">1 disabled</p>`,
		`<p class="cardsub">1 targets up</p>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing checks-card marker %q", want)
		}
	}
}

// TestDashboardChecksCardCalm: with nothing registered the card shows the calm
// line and no attention accent, and the count is never a bare zero.
func TestDashboardChecksCardCalm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	if !strings.Contains(page, `<a class="card cardlink" href="/checks" aria-labelledby="checks-h">`) {
		t.Error("checks card should render without the attn accent when nothing is registered")
	}
	if !strings.Contains(page, `<p class="cardsub meta">No checks registered.</p>`) {
		t.Error(`checks card should show "No checks registered." when nothing is registered`)
	}
}

// TestDashboardWorkflowsCardShowsWhenInFlight asserts the workflows card
// appears — and links to /workflows — exactly when a workflow is in flight,
// showing its count rather than the chain's id or step ledger.
func TestDashboardWorkflowsCardShowsWhenInFlight(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "provision", Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/sleep", "30"},
	})
	if err != nil {
		t.Fatalf("enqueue workflow step job: %v", err)
	}
	h.waitJobState(t, job.ID, store.StateRunning, 3*time.Second)

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

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`<a class="card cardlink" href="/workflows" aria-labelledby="flows-h">`,
		`<h3 id="flows-h">Workflows in flight</h3>`,
		`<div class="cardnum">1</div>`,
		`<p class="cardlabel">in flight</p>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing workflows-card marker %q", want)
		}
	}
	if strings.Contains(page, wfID) {
		t.Error("dashboard should show the workflow count, not its id")
	}
}

// TestDashboardSchedulesCard asserts the schedule card: the registered total
// and the "N schedules · M disabled" tally, with the attention accent exactly
// when a schedule is disabled — a disabled schedule has silently stopped
// firing. The full list (cron, next/last fire, outcome) all live on
// /schedules, not the glance screen.
func TestDashboardSchedulesCard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, name := range []string{"nightly-backup", "config-drift"} {
		if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
			Name: name, CronExpr: "0 3 * * *", Kind: "backup",
		}); err != nil {
			t.Fatalf("seed schedule %s: %v", name, err)
		}
	}
	if err := h.store.SetScheduleEnabled(ctx, "config-drift", false); err != nil {
		t.Fatalf("disable schedule: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`<a class="card cardlink attn" href="/schedules" aria-labelledby="sched-h">`,
		`<div class="cardnum">2</div>`,
		`<p class="cardlabel">schedules</p>`,
		`<p class="cardsub warn">2 schedules · 1 disabled</p>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing schedules-card marker %q", want)
		}
	}
	for _, banned := range []string{
		"nightly-backup",  // the schedule name — only its count shows now
		"config-drift",    // the disabled schedule's name — only its count shows now
		"0 3 * * *",       // the cron column is a /schedules concern
		"All schedules →", // the card is the link; no trailing text link
	} {
		if strings.Contains(page, banned) {
			t.Errorf("dashboard should summarise, not list schedules — found %q", banned)
		}
	}
}

// TestDashboardSchedulesCardCalm: with nothing registered the card shows the
// calm line and no attention accent, and the count is never a bare zero.
func TestDashboardSchedulesCardCalm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	if !strings.Contains(page, `<a class="card cardlink" href="/schedules" aria-labelledby="sched-h">`) {
		t.Error("schedules card should render without the attn accent when nothing is registered")
	}
	if !strings.Contains(page, `<p class="cardsub meta">No schedules registered.</p>`) {
		t.Error(`schedules card should show "No schedules registered." when nothing is registered`)
	}
}
