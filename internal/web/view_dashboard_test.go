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
	// Schedules card (the one table the redesign left untouched), so its
	// absence here proves every other card renders as a summary, not a list.
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
