package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// TestPowerlineSignalSegments is the GATE for the F10 realign: each powerline
// segment is dot + word in its signal color and links to where it is fixed, so
// a gap reads as a verb, not a grey fact. A fresh harness has no backup (dead-man
// none → wait), no push transport (ntfy off → deliberate dim), and no data dir
// (disk —, no state class). The dead-man gap reads bare `dead-man none` — the
// "— set one" suffix left with m96 (mutation: re-adding it reds this test).
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
		`— set one`,
	} {
		if strings.Contains(page, gone) {
			t.Errorf("powerline still carries the old grey-fact markup %q", gone)
		}
	}
}

// TestDashboardSummaryCards is the GATE for the at-a-glance redesign: the
// dashboard shows summary cards — counts plus a single attention detail —
// never an embedded table, and the queue card answers the glance with the
// active rows plus the LAST N RUNS group (SPEC §3), never a full history.
// A pending approval is the primary attention item and carries the wait/amber
// accent with a one-line subline naming the soonest-to-expire item.
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
	gated, approval := h.gatedJob(t, 30*time.Minute)

	// Settled history: the newest settled job lands in the LAST N RUNS group,
	// and nothing older than the group's window reaches the page.
	settled, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "settled-history-job", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("enqueue settled job: %v", err)
	}
	h.waitJobState(t, settled.ID, store.StateSucceeded, 3*time.Second)

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

	// Pending approvals: the primary attention item, a wait chip + one subline
	// naming the soonest item (the only one here) + the decision link.
	for _, want := range []string{
		`<a class="card cardlink attn" href="/approvals" aria-labelledby="needsyou-chip">`,
		`<span class="chip wait" id="needsyou-chip"><span class="dot"></span>1 PENDING</span>`,
		`Open approvals →`,
		"deploy-app", // gatedJob's fixed runbook — the soonest (only) item's target
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing approvals attention marker %q", want)
		}
	}
	_ = approval // the ID isn't asserted; only its effect on the card is

	// Queue section: the section label names the live queue's state (the
	// "In the queue" heading and its "all jobs →" link are gone — the rows
	// link to the jobs themselves), the active jobs render as /jobs two-line
	// rows (each its own link), and the newest settled job appears in the
	// LAST N RUNS group.
	for _, want := range []string{
		`1 running, oldest queued `,
		`<a class="jobs-row queue-row" href="/jobs/` + itoa64(running.ID) + `">`,
		`<span class="chip run"><span class="dot"></span>running</span>`,
		`<a class="jobs-row queue-row" href="/jobs/` + itoa64(gated.ID) + `">`,
		`<span class="chip wait"><span class="dot"></span>awaiting_approval</span>`,
		`LAST `,
		`RUNS`,
		"settled-history-job", // the newest settled job is the LAST RUNS group
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing queue marker %q", want)
		}
	}
	// The queue section label carries no heading or "all jobs" link of its
	// own; the rows are the links to the jobs (N1).
	for _, gone := range []string{"In the queue", "all jobs →"} {
		if strings.Contains(page, gone) {
			t.Errorf("dashboard queue label still carries %q", gone)
		}
	}

	// Workspaces: nothing registered, so it stays calm and lands in the COUNTS
	// band as a quiet tile naming the verb that changes it. Sources (calm) and
	// Checks (calm) render nowhere — quiet surfaces stay off the bands (F6).
	for _, want := range []string{
		`<a class="tile" href="/workspaces">`,
		`exocomp ws add`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing calm count marker %q", want)
		}
	}
	if strings.Contains(page, `<a class="tile" href="/sources">`) {
		t.Error("a calm sources surface should not render on the glance screen (F6)")
	}

	// Workflows: nothing in flight — the card is noise when it's usually
	// empty, so it is hidden entirely rather than shown as a permanent zero.
	if strings.Contains(page, `aria-labelledby="flows-h"`) {
		t.Error("workflows card should be hidden when nothing is in flight")
	}
}

// TestDashboardApprovalsCardCalm asserts the pending-approvals card's calm
// state: with nothing pending, the approvals card stays off the attention band
// and shows as a quiet tile in the COUNTS band ("none waiting"), never a bare
// zero with the attention accent.
func TestDashboardApprovalsCardCalm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`<a class="tile" href="/approvals">`,
		"none waiting",
		`○ QUEUE IDLE`,
		`Nothing queued, nothing running. Every run — manual, scheduled, reactive — lands here first.`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing approvals count marker %q", want)
		}
	}
	// The queue's idle fact is the card's own "○ QUEUE IDLE" line; the section
	// label never floats a bare "idle" word above the card (review P1).
	if strings.Contains(page, `<span>idle</span>`) {
		t.Error("queue section label still floats the bare idle word above the card")
	}
	if strings.Contains(page, "In the queue") || strings.Contains(page, "all jobs →") {
		t.Error("dashboard queue label still carries the heading or all-jobs link (N1)")
	}
	if strings.Contains(page, `aria-labelledby="needsyou-h"`) {
		t.Error("a calm approvals card should not render in the attention band (F6)")
	}
	if strings.Contains(page, "cardlink attn") {
		t.Error("no card should carry the attention accent on an otherwise-empty estate")
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
		`<a class="card cardlink attn" href="/checks" aria-labelledby="checks-chip">`,
		`<span class="chip wait" id="checks-chip"><span class="dot"></span>1 DISABLED</span>`,
		`Open checks →`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing checks-card marker %q", want)
		}
	}
}

// TestDashboardChecksCardCalm: with nothing registered the checks estate stays
// off the glance screen entirely — no attention band card, no count — because
// quiet surfaces render nothing (F6: healthy is quiet).
func TestDashboardChecksCardCalm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	if strings.Contains(page, `count-kicker">Checks`) || strings.Contains(page, `aria-labelledby="checks-h"`) {
		t.Error("a calm checks estate should not render on the glance screen (F6)")
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
		`<a class="card cardlink attn" href="/workflows" aria-labelledby="flows-chip">`,
		`<span class="chip wait" id="flows-chip"><span class="dot"></span>1 IN FLIGHT</span>`,
		`Open workflows →`,
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
		`<a class="card cardlink attn" href="/schedules" aria-labelledby="sched-chip">`,
		`<span class="chip wait" id="sched-chip"><span class="dot"></span>1 DISABLED</span>`,
		`Open schedules →`,
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

// TestDashboardSchedulesCardCalm: with nothing registered the schedules estate
// stays off the attention band and shows as a quiet count in the COUNTS band,
// never a bare zero with the accent.
func TestDashboardSchedulesCardCalm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	if !strings.Contains(page, `<a class="tile" href="/schedules">`) {
		t.Error("schedules tile should render in the COUNTS band when nothing is registered (F6)")
	}
	if strings.Contains(page, `aria-labelledby="sched-h"`) {
		t.Error("a calm schedules card should not render in the attention band (F6)")
	}
}

// TestDashboardChecksDownInBand is the GATE for W2: a check estate with targets
// down surfaces in the NEEDS ATTENTION band as a fail chip + sentence + decision
// link — the rail badge counts down targets and the band shares that one source
// of truth, so the "4 DOWN" fact is never buried.
func TestDashboardChecksDownInBand(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "presence", Kind: store.CheckTCP, IntervalS: 5,
		Targets: []store.CheckTarget{
			{Name: "phone", Address: "192.168.1.50", Port: 22},
			{Name: "nas", Address: "10.0.0.5", Port: 22},
		},
	})
	if err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	now := time.Now().UTC()
	for _, tn := range []string{"phone", "nas"} {
		if err := h.store.SetCheckState(ctx, store.CheckState{
			CheckID: c.ID, TargetName: tn, Status: store.CheckDown,
			Consecutive: 3, Emitted: store.CheckDown, FirstSeen: now, LastChange: now,
		}); err != nil {
			t.Fatalf("SetCheckState %s: %v", tn, err)
		}
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`<span class="chip fail" id="checks-chip"><span class="dot"></span>2 DOWN</span>`,
		`2 of 2 targets down`,
		`Open checks →`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing W2 checks-down marker %q", want)
		}
	}
}

// TestDashboardQueueCardStatesNoSettledFact pins H5: the queue card states
// only what the glance actually knows — the active queue. It never asserts a
// "last job" fact it does not query, so the hardcoded "last job — none yet"
// line is gone.
func TestDashboardQueueCardStatesNoSettledFact(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	if strings.Contains(page, "last job — none yet") {
		t.Error("queue card states a settled-job fact the glance does not query (H5)")
	}
}

// degradedSeedJob writes a finished tolerated ansible run straight into the
// store: degraded state plus the partial outcome naming the unreachable hosts,
// the shape a --tolerate-unreachable converge leaves behind. It never goes
// through the harness queue, so it must share a concurrency key with a running
// job to stay unclaimable while it is seeded.
func (h *harness) degradedSeedJob(t *testing.T, runbook, key string, hosts []string) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: runbook, Kind: executor.KindAnsible, Trigger: "schedule",
		ConcurrencyKey: key, Args: store.JobArgs{Argv: executor.AnsibleArtifactArgv("site.yml")},
	})
	if err != nil {
		t.Fatalf("create degraded job: %v", err)
	}
	raw, err := json.Marshal(map[string]executor.AnsiblePartialOutcome{
		"ansible_partial": {Operation: "run", Unreachable: hosts},
	})
	if err != nil {
		t.Fatalf("encode partial outcome: %v", err)
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelSystem, Msg: "ansible: degraded — tolerated",
		RawJSON: string(raw),
	}); err != nil {
		t.Fatalf("append partial outcome: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("start degraded job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateDegraded); err != nil {
		t.Fatalf("settle degraded job: %v", err)
	}
	return job
}

// TestDashboardQueueRecentRuns pins the queue card's LAST N RUNS group with a
// live queue (SPEC §3): the group shrinks to the newest two rows, the rule row
// reads "LAST 2 RUNS" with the "{n} FAILED" tally in the fail colour, and the
// Rerun verb renders on FAILED rows only — as a sibling link opening the rerun
// dialog via /jobs/{id}?rerun=1, never on a settled-but-quiet row.
func TestDashboardQueueRecentRuns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A live queue: one long-running job, so the recent group is capped at 2.
	running, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "live-job", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/sleep", "30"},
	})
	if err != nil {
		t.Fatalf("enqueue live job: %v", err)
	}
	h.waitJobState(t, running.ID, store.StateRunning, 3*time.Second)

	// Three settled jobs, oldest first: one clean run, then two failures. The
	// two newest are both failures, so "LAST 2 RUNS" reads "2 FAILED" and the
	// clean run falls outside the group's window.
	ok, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "older-clean", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("enqueue older clean run: %v", err)
	}
	h.waitJobState(t, ok.ID, store.StateSucceeded, 3*time.Second)

	fail := func(runbook string) store.Job {
		t.Helper()
		j, err := h.queue.Enqueue(ctx, queue.Spec{
			Runbook: runbook, Kind: "shell", Trigger: "manual",
			Argv: []string{"/bin/sh", "-c", "exit 1"},
		})
		if err != nil {
			t.Fatalf("enqueue failing run %s: %v", runbook, err)
		}
		h.waitJobState(t, j.ID, store.StateFailed, 3*time.Second)
		return j
	}
	failed1 := fail("recent-fail-1")
	failed2 := fail("recent-fail-2")

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`LAST 2 RUNS`,
		`<em class="failed">2 FAILED</em>`,
		`href="/jobs/` + itoa64(failed2.ID) + `?rerun=1"`,
		`href="/jobs/` + itoa64(failed1.ID) + `?rerun=1"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard queue rule/verb missing %q", want)
		}
	}
	// The live queue caps the group at two: "LAST 3 RUNS" must not render, and
	// the clean run (the oldest settled job) falls outside the group.
	if strings.Contains(page, "LAST 3 RUNS") {
		t.Error("queue rule shows LAST 3 RUNS with a live queue; the group must shrink to two")
	}
	if strings.Contains(page, "older-clean") {
		t.Error("queue recent group shows more than the newest two with a live queue")
	}
	// Exactly the two recent rows render; the live queue shows no idle line.
	if n := strings.Count(page, `<div class="queue-row">`); n != 2 {
		t.Errorf("queue recent group renders %d rows, want 2:\n%s", n, page)
	}
	if strings.Contains(page, "○ QUEUE IDLE") {
		t.Error("queue idle line rendered while the queue is live")
	}
}

// TestDashboardQueueRecentRunsIdle pins the recent group on an idle queue and
// the Rerun verb's state gate: "LAST 3 RUNS" renders, and the verb appears on
// FAILED rows only — a SUCCEEDED row is quiet, no verb, no reason line.
func TestDashboardQueueRecentRunsIdle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Three settled jobs, oldest first: two failures then a clean run, so the
	// newest settled run is the SUCCEEDED row the verb-gate must leave quiet.
	fail := func(runbook string) store.Job {
		t.Helper()
		j, err := h.queue.Enqueue(ctx, queue.Spec{
			Runbook: runbook, Kind: "shell", Trigger: "manual",
			Argv: []string{"/bin/sh", "-c", "exit 1"},
		})
		if err != nil {
			t.Fatalf("enqueue failing run %s: %v", runbook, err)
		}
		h.waitJobState(t, j.ID, store.StateFailed, 3*time.Second)
		return j
	}
	failed1 := fail("older-fail-1")
	failed2 := fail("older-fail-2")
	ok, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "newest-clean", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("enqueue newest clean run: %v", err)
	}
	h.waitJobState(t, ok.ID, store.StateSucceeded, 3*time.Second)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`LAST 3 RUNS`,
		`<em class="failed">2 FAILED</em>`,
		`href="/jobs/` + itoa64(failed2.ID) + `?rerun=1"`,
		`href="/jobs/` + itoa64(failed1.ID) + `?rerun=1"`,
		"newest-clean", // the newest settled run is in the group
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard queue recent group missing %q", want)
		}
	}
	// The SUCCEEDED row is quiet: no Rerun verb on its own id, no reason line.
	if strings.Contains(page, `href="/jobs/`+itoa64(ok.ID)+`?rerun=1"`) {
		t.Error("Rerun verb rendered on a SUCCEEDED row; the verb is FAILED/DEGRADED only")
	}
	if strings.Contains(page, `href="/jobs/`+itoa64(ok.ID)+`"`) == false {
		t.Error("succeeded recent row is missing its link to the job")
	}
	if strings.Contains(page, "○ QUEUE IDLE") == false {
		t.Error("idle queue must render the queue-idle line")
	}
}

// TestDashboardQueueRowTwoLine pins the queue card's phone row form (review
// P1): every queue row carries the two .line wrappers the 719 block stacks —
// line 1 the playbook/name cell with the state chip, line 2 the qmeta/qtime
// phone nodes — so the five desktop cells never pile up on phone. The harness
// cannot switch viewports, so the guard pins the markup the phone CSS consumes.
func TestDashboardQueueRowTwoLine(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	running, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "live-job", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/sleep", "30"},
	})
	if err != nil {
		t.Fatalf("enqueue live job: %v", err)
	}
	h.waitJobState(t, running.ID, store.StateRunning, 3*time.Second)

	var newestFail int64
	for _, runbook := range []string{"older-fail", "recent-fail"} {
		j, err := h.queue.Enqueue(ctx, queue.Spec{
			Runbook: runbook, Kind: "shell", Trigger: "manual",
			Argv: []string{"/bin/sh", "-c", "exit 1"},
		})
		if err != nil {
			t.Fatalf("enqueue failing run %s: %v", runbook, err)
		}
		h.waitJobState(t, j.ID, store.StateFailed, 3*time.Second)
		newestFail = j.ID
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}

	// Active row: the two-line convention's first .line opens the row link.
	if !strings.Contains(page, `<a class="jobs-row queue-row" href="/jobs/`+itoa64(running.ID)+`"><span class="line">`) {
		t.Error("active queue row is missing the .line wrapper the phone two-line form needs")
	}
	// Recent rows: each is two .line rows (playbook · chip over meta · time).
	if n := strings.Count(page, `<div class="queue-row"><span class="line">`); n != 2 {
		t.Errorf("queue recent rows render %d first-lines, want 2:\n%s", n, page)
	}
	// The state chip lives in its own cell on the first line.
	if !strings.Contains(page, `<a class="cell cell--state" href="/jobs/`+itoa64(newestFail)+`">`) {
		t.Error("recent row missing the chip cell that line 1 carries")
	}
	// The qmeta/qtime phone nodes are the second line's meta cells.
	if n := strings.Count(page, `<a class="cell cell--qmeta" href="/jobs/`); n != 2 {
		t.Errorf("recent rows render %d qmeta cells, want 2:\n%s", n, page)
	}
	if n := strings.Count(page, `<a class="cell cell--qtime" href="/jobs/`); n != 2 {
		t.Errorf("recent rows render %d qtime cells, want 2:\n%s", n, page)
	}
	// The qmeta carries the settled "#id · reason" fact for the newest row.
	if !strings.Contains(page, `<span class="qmeta">#`+itoa64(newestFail)+` · `) {
		t.Error("recent row qmeta is missing the #id · reason fact")
	}
}

// TestDashboardQueueRowsCapped pins the queue card's live-row glance ceiling:
// the rows stop at the cap (queueRowsCap, six today), newest first, while the
// section label still states the whole active set — the oldest queued age
// among them. The full queue lives on /jobs, reached through the all-jobs link
// — the card never becomes a list.
func TestDashboardQueueRowsCapped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Eight gated (awaiting_approval) jobs — all active, none claimable by a
	// worker — one more than the cap allows past the oldest dropped row.
	for i := 0; i < 8; i++ {
		if _, err := h.store.CreateGatedJob(ctx, store.NewJob{
			Runbook: "cap-job-" + itoa(int64(i)), Kind: "shell", Trigger: "manual",
			Args: store.JobArgs{Argv: []string{"/bin/true"}},
		}); err != nil {
			t.Fatalf("seed capped job %d: %v", i, err)
		}
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	// Exactly the cap's live rows render, newest first: the last-seeded job
	// shows, the first-seeded one is the one the cap drops.
	if n := strings.Count(page, `class="jobs-row queue-row"`); n != 6 {
		t.Errorf("queue card renders %d rows, want the cap of 6:\n%s", n, page)
	}
	if !strings.Contains(page, "cap-job-7") {
		t.Error("newest active row must render (cap-job-7)")
	}
	if strings.Contains(page, "cap-job-0") {
		t.Error("oldest active row must drop off the capped card (cap-job-0)")
	}
	// The section label states the whole active set, so a queue past the cap
	// still reads in full on the glance screen — all queued here, so "0
	// running, oldest queued {age}".
	if !strings.Contains(page, `0 running, oldest queued `) {
		t.Error("queue section label must state the full active set past the cap")
	}
}

// TestDashboardCountCardHidesZeroVerbWhenNonzero pins the count-tile fix: the
// zero-state sentence (the verb that changes it) renders only when the count
// is zero — a nonzero count must not wear its own zero-state copy.
func TestDashboardCountCardHidesZeroVerbWhenNonzero(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateWorkspace(ctx, store.Workspace{Name: "prod", TofuDir: "prod"}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	if !strings.Contains(page, `<a class="tile" href="/workspaces">`) {
		t.Error("workspaces count tile missing")
	}
	if !strings.Contains(page, `<b>1</b>`) {
		t.Error("workspaces count should read 1")
	}
	if strings.Contains(page, "exocomp ws add") {
		t.Error("nonzero workspaces count still renders the zero-state verb")
	}
}

// seedWebBackupRun registers one backup run of the given state, finished at
// the given time, so the dead-man web test controls history exactly.
func seedWebBackupRun(t *testing.T, h *harness, targetID int64, state string, finishedAt time.Time) error {
	t.Helper()
	ctx := context.Background()
	run, err := h.store.StartBackupRun(ctx, targetID)
	if err != nil {
		return err
	}
	run.State = state
	run.FinishedAt = &finishedAt
	if state == store.BackupSucceeded {
		run.SnapshotID = "snap"
	}
	return h.store.FinishBackupRun(ctx, run)
}

// TestDeadManReportsOlderSucceededRun pins H6: the powerline dead-man reads
// the store's ONE rule (LastSucceededBackupTime) — succeeded runs only, always
// finished. A FAILED newest run must never mask an older good backup or report
// "none"; it reports the older succeeded run's age.
func TestDeadManReportsOlderSucceededRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	target, err := h.store.CreateBackupTarget(ctx, store.BackupTarget{
		Name: "nas", Kind: "restic_local", URI: "/srv/nas", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create backup target: %v", err)
	}
	good := time.Now().Add(-3 * time.Hour).Add(-15 * time.Minute).UTC()
	if err := seedWebBackupRun(t, h, target.ID, store.BackupSucceeded, good); err != nil {
		t.Fatalf("seed succeeded run: %v", err)
	}
	bad := time.Now().Add(-30 * time.Minute).UTC()
	if err := seedWebBackupRun(t, h, target.ID, store.BackupFailed, bad); err != nil {
		t.Fatalf("seed failed run: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	if !strings.Contains(page, "dead-man 3h") {
		t.Errorf("dead-man must report the older succeeded run's age (3h), got:\n%s", page)
	}
	if strings.Contains(page, "dead-man none") {
		t.Error("dead-man reported none despite an older succeeded run (H6)")
	}
}

// TestDashboardAppbarMetaNotIdentity is the GATE for W7: the app bar's meta
// slot carries the page's own fact (host · uptime), and the signed-in identity
// appears only in the powerline — never twice.
func TestDashboardAppbarMetaNotIdentity(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	if !strings.Contains(page, `class="appbar-meta">`) {
		t.Error("dashboard app bar has no page-own meta slot (W7)")
	}
	if strings.Contains(page, `class="appbar-meta">laptop · operator`) {
		t.Error("app bar meta still carries the identity (W7)")
	}
	if !strings.Contains(page, `class="pl-who"`) {
		t.Error("identity must live in the powerline")
	}
	if !strings.Contains(page, "up ") {
		t.Error("dashboard meta should name the uptime")
	}
}

// TestInsecureBannerHumanCopy is the GATE for the operator's UX flag: the
// insecure-HTTP banner and the login warning read as plain English, not
// internal vocabulary ("cleartext", "on the wire", "listener"). They render
// only under --insecure-http, so the copy is pinned by rendering the templates
// directly with the insecure flag set.
func TestInsecureBannerHumanCopy(t *testing.T) {
	var buf bytes.Buffer
	vm := dashboardVM{Page: page{Insecure: true}}
	if err := dashboard(vm).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render dashboard: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`▲ INSECURE HTTP`,
		`Anyone on this network can read your sign-in.`,
		`Serve over TLS →`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard banner missing %q", want)
		}
	}
	for _, gone := range []string{"cleartext", "on the wire", "served over", "This page is not secure"} {
		if strings.Contains(out, gone) {
			t.Errorf("dashboard banner still uses internal vocabulary %q", gone)
		}
	}

	buf.Reset()
	if err := loginPage("", "", "", true, "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render loginPage: %v", err)
	}
	login := buf.String()
	for _, want := range []string{
		`This sign-in is not secure. Anyone on this network could read it.`,
	} {
		if !strings.Contains(login, want) {
			t.Errorf("login insecure warning missing %q", want)
		}
	}
}

// TestDashboardInsecureBannerVerb pins R3: the insecure banner carries the
// ▲ keyword, the one sentence, and the "Serve over TLS →" verb as a link —
// a banner with a decision, never a two-line fact.
func TestDashboardInsecureBannerVerb(t *testing.T) {
	var buf bytes.Buffer
	vm := dashboardVM{Page: page{Insecure: true}}
	if err := dashboard(vm).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render dashboard: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `▲ INSECURE HTTP`) {
		t.Errorf("banner keyword missing the ▲ marker:\n%s", out)
	}
	if !strings.Contains(out, `Anyone on this network can read your sign-in.`) {
		t.Errorf("banner sentence missing:\n%s", out)
	}
	if !strings.Contains(out, `<a class="verb" href="https://" data-tls data-tls-base="">Serve over TLS →</a>`) {
		t.Errorf("banner verb must be the Serve-over-TLS link:\n%s", out)
	}
	var buf2 bytes.Buffer
	vm2 := dashboardVM{Page: page{Insecure: true, Base: "/exocomp"}}
	if err := dashboard(vm2).Render(context.Background(), &buf2); err != nil {
		t.Fatalf("render dashboard: %v", err)
	}
	if !strings.Contains(buf2.String(), `data-tls-base="/exocomp"`) {
		t.Errorf("banner TLS verb must carry the surface's base path")
	}
}

// TestDashboardCountsTiles pins the counts band's 2x2 tiles (SPEC §2.1): four
// whole-tile links to their full views, the value, and — only at zero — the
// verbatim zero-state copy naming the CLI verb that changes the count.
func TestDashboardCountsTiles(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`<a class="tile" href="/approvals">`,
		`<a class="tile" href="/workspaces">`,
		`<a class="tile" href="/schedules">`,
		`<a class="tile" href="/rules">`,
		`none waiting`,
		`exocomp ws add`,
		`exocomp schedule add`,
		`exocomp rule add`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard counts tiles missing %q", want)
		}
	}
	// Four tiles, each the whole link to its full view.
	if n := strings.Count(page, `<a class="tile" href=`); n != 4 {
		t.Errorf("dashboard renders %d count tiles, want 4:\n%s", n, page)
	}
}
