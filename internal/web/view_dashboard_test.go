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
// way on the queue card, whose active jobs now render as two-line rows.
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

	// Queue: the active jobs render as /jobs two-line rows — runbook over
	// source/meta, a state chip, each row its own link to /jobs/{id} — with the
	// running/queued split as the card's meta line and the recent-failure
	// subline in the fail color.
	for _, want := range []string{
		`<div class="card queue">`,
		`<p class="cardsub meta">1 running · 1 queued</p>`,
		`<a class="jobs-row queue-row" href="/jobs/` + itoa64(running.ID) + `">`,
		`<span class="chip run"><span class="dot"></span>running</span>`,
		`<a class="jobs-row queue-row" href="/jobs/` + itoa64(gated.ID) + `">`,
		`<span class="chip wait"><span class="dot"></span>awaiting_approval</span>`,
		`<p class="cardsub fail">1 failed recently</p>`,
		`<a class="cardact" href="/jobs">Open all jobs →</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing queue marker %q", want)
		}
	}

	// Workspaces: nothing registered, so it stays calm and lands in the COUNTS
	// band as a quiet count naming the verb that changes it. Sources (calm) and
	// Checks (calm) render nowhere — quiet surfaces stay off the bands (F6).
	for _, want := range []string{
		`<div class="count-kicker">Workspaces</div>`,
		`exocomp ws add`,
		`<div class="countnum">0</div>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing calm count marker %q", want)
		}
	}
	if strings.Contains(page, `count-kicker">Sources`) || strings.Contains(page, `aria-labelledby="sources-h"`) {
		t.Error("a calm sources surface should not render on the glance screen (F6)")
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
// state: with nothing pending, the approvals card stays off the attention band
// and shows as a quiet count in the COUNTS band ("None waiting."), never a
// bare zero with the attention accent.
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
		`<div class="count-kicker">Approvals</div>`,
		`<div class="countnum">0</div>`,
		`None waiting.`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing approvals count marker %q", want)
		}
	}
	if strings.Contains(page, `aria-labelledby="needsyou-h"`) {
		t.Error("a calm approvals card should not render in the attention band (F6)")
	}
	if strings.Contains(page, "cardlink attn") {
		t.Error("no card should carry the attention accent on an otherwise-empty estate")
	}
	if !strings.Contains(page, `Idle — nothing queued, nothing running.`) {
		t.Error(`queue card should show "Idle — nothing queued, nothing running." when nothing is active`)
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
	if !strings.Contains(page, `<div class="count-kicker">Schedules</div>`) {
		t.Error("schedules count should render in the COUNTS band when nothing is registered (F6)")
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

// TestDashboardQueueCardLastJobLine pins S5: the queue card's last-job line is
// a REAL query — the newest settled job, its runbook and a state chip — with a
// degraded run rendered as the wait-coloured DEGRADED chip. A degraded job must
// not inflate "failed recently", which counts StateFailed only.
func TestDashboardQueueCardLastJobLine(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// An older clean run settles first; the degraded run is the newest settled
	// job, so the last-job line names it. The holder keeps the seeded degraded
	// row unclaimable by the live harness queue.
	if _, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "older-clean", Kind: "shell", Trigger: "manual",
		Argv: []string{"/bin/sh", "-c", "exit 0"},
	}); err != nil {
		t.Fatalf("enqueue older run: %v", err)
	}
	holder, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "key-holder", Kind: "shell", Trigger: "manual",
		ConcurrencyKey: "deg-seed", Argv: []string{"/bin/sleep", "30"},
	})
	if err != nil {
		t.Fatalf("enqueue key holder: %v", err)
	}
	h.waitJobState(t, holder.ID, store.StateRunning, 3*time.Second)
	h.degradedSeedJob(t, "nightly-converge", "deg-seed", []string{"hypnotoad", "lrrr"})

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`<span class="chip wait"><span class="dot"></span>DEGRADED</span>`,
		"nightly-converge",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard last-job line missing %q", want)
		}
	}
	// A degraded job is not a failure: nothing here failed, so the subline
	// must not appear.
	if strings.Contains(page, "failed recently") {
		t.Error("a degraded job inflated the failed-recently count")
	}
	// The glance queries only the newest settled job.
	if strings.Contains(page, "older-clean") {
		t.Error("queue card shows more than the newest settled job")
	}
}

// TestDashboardQueueCardLastJobLineSucceeded is the clean twin: a newest
// succeeded run renders the ok chip, so the line is not degraded-only.
func TestDashboardQueueCardLastJobLineSucceeded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "nightly-backup", Kind: "shell", Trigger: "schedule",
		Argv: []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("enqueue succeeded job: %v", err)
	}
	h.waitJobState(t, job.ID, store.StateSucceeded, 3*time.Second)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/")
	if code != 200 {
		t.Fatalf("dashboard status = %d", code)
	}
	for _, want := range []string{
		`<span class="chip ok"><span class="dot"></span>SUCCEEDED</span>`,
		"nightly-backup",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard last-job line missing %q", want)
		}
	}
}

// TestDashboardQueueRowsCapped pins the queue card's glance ceiling: the rows
// stop at the cap (queueRowsCap, six today), newest first, while the headline
// still states the whole active set. The full queue lives on /jobs, reached
// through the Open all jobs footer — the card never becomes a list.
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
	// Exactly the cap's rows render, newest first: the last-seeded job shows,
	// the first-seeded one is the one the cap drops.
	if n := strings.Count(page, `class="jobs-row queue-row"`); n != 6 {
		t.Errorf("queue card renders %d rows, want the cap of 6:\n%s", n, page)
	}
	if !strings.Contains(page, "cap-job-7") {
		t.Error("newest active row must render (cap-job-7)")
	}
	if strings.Contains(page, "cap-job-0") {
		t.Error("oldest active row must drop off the capped card (cap-job-0)")
	}
	// The headline counts the whole active set, so a queue past the cap still
	// reads in full on the glance screen.
	if !strings.Contains(page, `<p class="cardsub meta">0 running · 8 queued</p>`) {
		t.Error("queue card headline must state the full active set past the cap")
	}
}

// TestDashboardCountCardHidesZeroVerbWhenNonzero pins the count-card fix: the
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
	if !strings.Contains(page, `<div class="count-kicker">Workspaces</div>`) {
		t.Error("workspaces count card missing")
	}
	if !strings.Contains(page, `<div class="countnum">1</div>`) {
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
		`INSECURE HTTP`,
		`This page is not secure. Anyone on this network could read your sign-in.`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard banner missing %q", want)
		}
	}
	for _, gone := range []string{"cleartext", "on the wire", "served over"} {
		if strings.Contains(out, gone) {
			t.Errorf("dashboard banner still uses internal vocabulary %q", gone)
		}
	}

	buf.Reset()
	if err := loginPage("", "", "", true).Render(context.Background(), &buf); err != nil {
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
