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

	// Queue: active-only counts, plus the recent-failure subline in the fail
	// color.
	for _, want := range []string{
		`<a class="card cardlink queue" href="/jobs" aria-labelledby="queue-h">`,
		`<div class="cardnum">1 running · 1 queued</div>`,
		`<p class="cardsub fail">1 failed recently</p>`,
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
