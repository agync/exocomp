package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// phone_r2_jobs_test.go — the phone-web round-2 job-surface gates: R3 (jobs
// row line 1, the exit-1 meta dedup, the dropped card heading), R4 (back
// chevron + ink colour, the dropped kind chip), R5 (wrap-by-default and the
// one log gutter), R6 (the single-tile run-config collapse). Layout is
// browser-verified at 390px; these pin the markup and the CSS the phone
// re-flow consumes.

// firstQMeta returns the first phones row's line-2 meta text (the fact after
// the "cli · operator · …" clock pieces). The jobs list renders one qmeta per
// row; the tests seed one row.
func firstQMeta(t *testing.T, body string) string {
	t.Helper()
	const open = `<span class="cell cell--meta"><span class="qmeta">`
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatal("jobs list missing the line-2 .cell--meta qmeta node")
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "</span>")
	if j < 0 {
		t.Fatal("unterminated qmeta node")
	}
	return rest[:j]
}

// seedBackupFailed drives a backup-kind job to a terminal failure: its only
// run-config tile is "Tolerate unreachable", and its kind ("backup") repeats
// its title. It carries no recap, so its D4 reason is the bare "exit 1".
func seedBackupFailed(t *testing.T, h *harness) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := h.store.CreateGatedJob(ctx, store.NewJob{
		Runbook: "backup", Kind: "backup", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"restic", "backup"}},
	})
	if err != nil {
		t.Fatalf("seed backup job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateAwaitingApproval, store.StateRunning); err != nil {
		t.Fatalf("start backup job: %v", err)
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{JobID: job.ID, Level: store.LevelSystem, Msg: "failed (exit 1)"}); err != nil {
		t.Fatalf("append band: %v", err)
	}
	if err := h.store.SetJobExitCode(ctx, job.ID, 1); err != nil {
		t.Fatalf("set exit: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateFailed); err != nil {
		t.Fatalf("finish backup job: %v", err)
	}
	return job
}

// TestPhoneJobsRowLineOneLeft is the R3 gate for line 1: `#id · title` reads
// LEFT (the number no longer floats) with the state chip pinned right through
// margin-left:auto. The phone block carries the # and · separators as generated
// content — the desktop # column keeps its bare number. The MUTATION — dropping
// the flex-start or the chip margin — reds this test.
func TestPhoneJobsRowLineOneLeft(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs")

	// The DOM keeps #id in .num then the title in .cell; the # and · are the
	// phone block's generated content.
	num := strings.Index(body, `<span class="num">`+itoa64(job.ID)+`</span>`)
	title := strings.Index(body, `<b class="mono">deploy/site.yml</b>`)
	if num < 0 || title < 0 || num > title {
		t.Errorf("line 1 must render #id before the title (num %d, title %d)", num, title)
	}

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.jobs-row, .queue-row { display: flex; flex-direction: column; align-items: stretch; gap: 5px; height: auto; padding: 11px 12px; }`,
		`.jobs-row:not(.queue-row) .line:first-child { justify-content: flex-start; }`,
		`.jobs-row:not(.queue-row) .line:first-child .num::before { content: "#"; }`,
		`.jobs-row:not(.queue-row) .line:first-child .num::after { content: " ·"; }`,
		`.jobs-row:not(.queue-row) .line:first-child .cell--state { flex: none; margin-left: auto; }`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the R3 line-1 rule %q", want)
		}
	}
}

// TestPhoneJobsMetaDropsExitOne is the R3 gate for the duplicated reason: a
// FAILED chip already says the run failed, so a bare "exit 1" drops from the
// line-2 meta; a reason that adds a cause (exit 4 · N hosts unreachable) stays.
// The MUTATION — always appending the reason — reds this test.
func TestPhoneJobsMetaDropsExitOne(t *testing.T) {
	// Bare exit 1: the meta must not restate it.
	h1 := newHarness(t)
	ts1 := httptest.NewServer(h1.server.Handler())
	defer ts1.Close()
	seedJobWithRecap(t, h1, store.StateFailed, 1, nil, nil, store.JobArgs{Argv: []string{"/bin/true"}})
	client1 := login(t, ts1.URL, h1.mint(t, "laptop", "operator"))
	_, body1 := getBody(t, client1, ts1.URL+"/jobs")
	if meta := firstQMeta(t, body1); strings.Contains(meta, "exit 1") {
		t.Errorf("the line-2 meta must drop the duplicate exit 1, got %q", meta)
	}

	// Exit 4 with a cause: the meta keeps it.
	h2 := newHarness(t)
	ts2 := httptest.NewServer(h2.server.Handler())
	defer ts2.Close()
	seedFailedWithRecap(t, h2, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
	})
	client2 := login(t, ts2.URL, h2.mint(t, "laptop", "operator"))
	_, body2 := getBody(t, client2, ts2.URL+"/jobs")
	if meta := firstQMeta(t, body2); !strings.Contains(meta, "exit 4") {
		t.Errorf("the line-2 meta must keep a non-1 exit reason, got %q", meta)
	}
}

// TestPhoneJobsListDropsHeading is the R3 gate for the card's dead air: the
// "All jobs" h3 restated the app-bar title and cost the first row ~130px along
// with the kicker and filter. The kicker stays; the heading goes. The MUTATION
// — restoring the h3 — reds this test.
func TestPhoneJobsListDropsHeading(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedFailedWithRecap(t, h, 4, []string{"PLAY RECAP"})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs")
	if strings.Contains(body, "<h3>All jobs</h3>") {
		t.Error("the jobs card's redundant 'All jobs' heading must be gone")
	}
	if !strings.Contains(body, `<div class="kicker">Run ledger</div>`) {
		t.Error("the jobs card must keep its Run ledger kicker")
	}
}

// TestPhoneAppBarBackChevron is the R4 gate for the app bar's back slot: the
// path was two parallel diagonals (a double-slash), not a chevron, and it
// inherited the a{accent-text} purple. The path is a joined chevron and the
// shell rule makes it ink. The MUTATION — restoring the double-slash or the
// colour rule — reds this test.
func TestPhoneAppBarBackChevron(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{"PLAY RECAP"})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if !strings.Contains(body, `class="back"`) {
		t.Fatal("job detail page missing the app-bar back link")
	}
	if !strings.Contains(body, `<path d="M15 5l-10 10 10 10"></path>`) {
		t.Error("the back slot must render the joined chevron path")
	}
	if strings.Contains(body, `M15 5l-10 10M14 5l-7 7`) {
		t.Error("the malformed double-slash back path must be gone")
	}

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	if !strings.Contains(app, `.appbar .back { color: var(--ink-200); }`) {
		t.Error("app.css must render the back chevron in ink, not the a{accent-text} purple")
	}
}

// TestPhoneRunCardDropsKindChip is the R4 gate for the run-card head: a
// backup job's kind chip reads "backup" over "backup job #N", repeating the
// title. The chip drops when it equals the title; a playbook job's "ansible"
// chip (a real distinction) stays. The MUTATION — rendering it unconditionally
// — reds this test.
func TestPhoneRunCardDropsKindChip(t *testing.T) {
	// The backup job: kind == title, so no kind chip.
	hb := newHarness(t)
	tsb := httptest.NewServer(hb.server.Handler())
	defer tsb.Close()
	bk := seedBackupFailed(t, hb)
	clientb := login(t, tsb.URL, hb.mint(t, "laptop", "operator"))
	_, backupBody := getBody(t, clientb, tsb.URL+"/jobs/"+itoa64(bk.ID))
	if strings.Contains(backupBody, `class="chip dim"><span class="dot"></span>backup</span>`) {
		t.Error("the backup kind chip repeats the title and must be dropped")
	}
	// The card no longer restates the title: the app bar's sub carries it, and
	// the card's own title span is gone.
	if strings.Contains(backupBody, `class="title mono">`) {
		t.Error("the run card must not restate the title the app bar already shows")
	}
	if !strings.Contains(backupBody, `class="appbar-sub">backup</span>`) {
		t.Error("the app bar must carry the backup title the run card no longer repeats")
	}

	// The playbook job: the tool (ansible) is not the title, so the chip stays.
	hp := newHarness(t)
	tsp := httptest.NewServer(hp.server.Handler())
	defer tsp.Close()
	pl := seedFailedWithRecap(t, hp, 4, []string{"PLAY RECAP"})
	clientp := login(t, tsp.URL, hp.mint(t, "laptop", "operator"))
	_, playBody := getBody(t, clientp, tsp.URL+"/jobs/"+itoa64(pl.ID))
	if !strings.Contains(playBody, `class="chip dim"><span class="dot"></span>ansible</span>`) {
		t.Error("a playbook run card must keep its ansible tool chip")
	}
}

// TestPhoneLogWrapDefaultAndGutter is the R5 gate: the 719 block wraps by
// default (the base .log is pre-wrap, and the capture that read raw was
// fine-pointer), so the .wrap toggle is the raw state; and the log lines carry
// one gutter — the desktop/phone grid comes from CSS, never a 70px inline that
// dead-letters the phone 60px rule. The MUTATION — restoring the inline style
// or the 719 raw pre — reds this test.
func TestPhoneLogWrapDefaultAndGutter(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if strings.Contains(body, `grid-template-columns:70px 1fr`) {
		t.Error("the log line must not inline a 70px grid (it dead-letters the phone gutter)")
	}
	if strings.Contains(body, `span.style.gridTemplateColumns`) {
		t.Error("the streaming JS must not inline the log grid; CSS owns the one gutter")
	}
	if !strings.Contains(body, `<span class="line"><span class="ts">`) {
		t.Error("the server log line must render bare, letting .log .line set the grid")
	}

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.log{max-height:60vh;white-space:pre-wrap; overflow:auto; font-size:12px; }`,
		`.log .line { grid-template-columns: 60px 1fr; width: auto; min-width: 100%; }`,
		`.log.wrap .line { white-space: pre; width: max-content; }`,
		`.output-head .wrap { height: 36px; background: transparent; border-color: transparent; box-shadow: none; }`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the R5 log rule %q", want)
		}
	}
	if strings.Contains(app, `.log.wrap .line { white-space: pre-wrap;`) {
		t.Error("the .wrap toggle must be the raw state, not the pre-wrap default")
	}
}

// TestPhoneRunConfigCollapsesSingleTile is the R6 gate: a run whose config is
// one tile reads as a single mono line under the queued-by sentence; a
// multi-tile run (a playbook's Source/Mode/…) keeps the RUN CONFIGURATION tile
// grid. The MUTATION — always rendering the tiles — reds the first assertion.
func TestPhoneRunConfigCollapsesSingleTile(t *testing.T) {
	// One tile (backup): the collapsed line, no grid.
	hb := newHarness(t)
	tsb := httptest.NewServer(hb.server.Handler())
	defer tsb.Close()
	bk := seedBackupFailed(t, hb)
	clientb := login(t, tsb.URL, hb.mint(t, "laptop", "operator"))
	_, backupBody := getBody(t, clientb, tsb.URL+"/jobs/"+itoa64(bk.ID))
	if !strings.Contains(backupBody, `<p class="config-line">Tolerate unreachable · off</p>`) {
		t.Errorf("a single-tile run config must collapse to one line:\n%s", backupBody)
	}
	if strings.Contains(backupBody, `<div class="config">`) {
		t.Error("a single-tile run config must not render the tile grid")
	}

	// Multiple tiles (a playbook with a check mode): the grid stays.
	hp := newHarness(t)
	tsp := httptest.NewServer(hp.server.Handler())
	defer tsp.Close()
	pl := seedFailedWithRecap(t, hp, 4, []string{"PLAY RECAP"})
	clientp := login(t, tsp.URL, hp.mint(t, "laptop", "operator"))
	_, playBody := getBody(t, clientp, tsp.URL+"/jobs/"+itoa64(pl.ID))
	if !strings.Contains(playBody, `<div class="config">`) {
		t.Error("a multi-tile run config must keep the tile grid")
	}
	if strings.Contains(playBody, `<p class="config-line">`) {
		t.Error("a multi-tile run config must not collapse to the one-line form")
	}

	_, app := getBody(t, clientb, tsb.URL+"/assets/app.css")
	if !strings.Contains(app, `.config-line { font: 400 12px var(--font-mono);`) {
		t.Error("app.css missing the R6 collapsed config line (12px mono)")
	}
}

// TestPhoneJobsMetaWrapsNotTruncates pins the line-2 meta's phone form: the
// flat mono run overran one phone line, so the ellipsis ate the duration off
// its tail. The meta wraps instead (overflow-wrap: anywhere, no text-overflow
// ellipsis), and the two-line row shape is unchanged — the primary line over
// the mono meta line. The MUTATION — restoring white-space: nowrap with the
// ellipsis — reds this test.
func TestPhoneJobsMetaWrapsNotTruncates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	meta := ruleDecl(app, ".jobs-row .line + .line .cell--meta {")
	if !strings.Contains(meta, "white-space: normal") || !strings.Contains(meta, "overflow-wrap: anywhere") {
		t.Errorf("the phone meta cell must wrap its facts, got: %q", meta)
	}
	if strings.Contains(meta, "text-overflow: ellipsis") {
		t.Errorf("the phone meta cell must never ellipsize a fact, got: %q", meta)
	}
	if !strings.Contains(app, `.jobs-row, .queue-row { display: flex; flex-direction: column; align-items: stretch; gap: 5px; height: auto; padding: 11px 12px; }`) {
		t.Error("the phone two-line row shape must stay")
	}
}
