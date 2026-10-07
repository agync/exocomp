package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// TestRunbookLabel: the list and detail read a rule-triggered job's runbook
// without the "rule:" logic prefix; a non-rule runbook passes through.
func TestRunbookLabel(t *testing.T) {
	for _, tc := range []struct {
		runbook, want string
	}{
		{"rule:gate-ssh", "gate-ssh"},
		{"playbook", "playbook"},
	} {
		j := jobRow{Runbook: tc.runbook}
		if got := j.RunbookLabel(); got != tc.want {
			t.Errorf("RunbookLabel(%q) = %q, want %q", tc.runbook, got, tc.want)
		}
	}
}

// TestJobDetailRuleProvenance: a rule-triggered job's detail view links back to
// the rule its runbook named and to the event that raised it.
func TestJobDetailRuleProvenance(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "rule:gate-ssh", Kind: "playbook", Trigger: "rule",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	jobID := job.ID
	ev, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", MatchedRule: "gate-ssh", Outcome: store.OutcomeFired,
		JobID: &jobID, Normalized: `{"name":"at-home"}`,
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	if code != 200 {
		t.Fatalf("job detail status = %d", code)
	}
	for _, want := range []string{
		`fired by rule <a href="/rules">gate-ssh</a>`,
		`event <a href="/events/` + itoa(ev.ID) + `">#` + itoa(ev.ID) + `</a>`,
		`(<span class="mono">check</span> at-home) at`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("job detail missing provenance %q", want)
		}
	}
}

// TestJobDetailRuleNoEvent: a rule-runbook job with no event row renders the
// rule line and nothing more — no fabricated event link.
func TestJobDetailRuleNoEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "rule:gate-ssh", Kind: "playbook", Trigger: "rule",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	if code != 200 {
		t.Fatalf("job detail status = %d", code)
	}
	if !strings.Contains(page, `fired by rule <a href="/rules">gate-ssh</a>`) {
		t.Error("job detail missing the fired-by-rule line")
	}
	if strings.Contains(page, "/events/") {
		t.Error("a job without an event row must not render an event link")
	}
}

// TestJobDetailNoContentBreadcrumb is review P6: the job detail page's
// in-content "Jobs / #4" breadcrumb is gone — the app bar already carries the
// back chevron to /jobs and the stacked title, so the content breadcrumb is a
// duplicate. Its absence is pinned here; the shared breadcrumb component stays
// for pages that still need it (workflows, events, sources).
func TestJobDetailNoContentBreadcrumb(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "manual-provision", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	if code != 200 {
		t.Fatalf("job detail status = %d", code)
	}
	for _, gone := range []string{
		`class="breadcrumb"`,
		`href="/jobs">Jobs</a>`,
		`aria-current="page">#` + itoa(job.ID) + `</span>`,
	} {
		if strings.Contains(page, gone) {
			t.Errorf("job detail still renders the content breadcrumb %q (P6)", gone)
		}
	}
	// The phone block hides the shared breadcrumb (the stray "/" separator)
	// while the page title STAYS in the bar (SPEC §2.0): the job page still
	// renders the stacked title-wrap, and app.css keeps it display:flex on
	// phone instead of hiding it with the breadcrumb.
	for _, want := range []string{
		`class="appbar-title-wrap">`,
		`<h1 class="appbar-title">Job #` + itoa(job.ID) + `</h1>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("job detail must keep the app-bar page title %q (SPEC §2.0)", want)
		}
	}
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.breadcrumb { display: none; }`,
		`.appbar-title-wrap { display: flex;`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the phone rule %q (P6)", want)
		}
	}
}

// TestWorkflowDetailBreadcrumb: the workflow ledger view carries a breadcrumb
// back to the workflows list.
func TestWorkflowDetailBreadcrumb(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.store.CreateWorkflow(ctx, "provision-web-01", []store.NewWorkflowStep{
		{StepNo: 1, Kind: "tofu-apply", IdempotencyKey: "k1"},
		{StepNo: 2, Kind: "ansible-converge", IdempotencyKey: "k2"},
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/workflows/provision-web-01")
	if code != 200 {
		t.Fatalf("workflow detail status = %d", code)
	}
	for _, want := range []string{
		`class="breadcrumb"`,
		`href="/workflows">Workflows</a>`,
		`aria-current="page">Workflow</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("workflow detail missing breadcrumb %q", want)
		}
	}
}

// TestJobDetailStaleBannerAndHeartbeat pins the MED stream-alignment fix: the
// job tail page carries the same stale-live-updates banner and reconnect
// handling the approvals tail documents — the banner element, an onerror that
// surfaces it only when the stream is closed for good, and an onopen that
// clears it — so a refused or ended tail is never silently dead.
func TestJobDetailStaleBannerAndHeartbeat(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	// A gated job stays awaiting_approval (non-terminal), so the detail page
	// renders the live tail script.
	job, err := h.queue.EnqueueGated(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("EnqueueGated: %v", err)
	}
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	if code != http.StatusOK {
		t.Fatalf("job detail status = %d", code)
	}
	for _, want := range []string{
		`id="jobs-stale"`,                      // the stale banner element
		`es.readyState === EventSource.CLOSED`, // surfaces only a closed stream
		`es.onopen`,                            // clears on reconnect
		`new EventSource(base + "/jobs/" + job + "/stream?since=" + since)`, // opens the tail with the cursor
		`data-base="`,       // the JS base seam
		`location.reload()`, // done reloads for true final state
	} {
		if !strings.Contains(page, want) {
			t.Errorf("job detail missing stale/heartbeat marker %q", want)
		}
	}
	if strings.Contains(page, "es.close()") {
		t.Error("job tail still closes the stream on error instead of letting EventSource reconnect")
	}
}

// TestJobDetailNoRule: a job whose runbook has no rule prefix shows no
// provenance line at all.
func TestJobDetailNoRule(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "manual-provision", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	if code != 200 {
		t.Fatalf("job detail status = %d", code)
	}
	if strings.Contains(page, "fired by rule") {
		t.Error("a non-rule job must not render a fired-by-rule line")
	}
}

// TestJobRawLogServesPlainText gates the "view raw log" button (Q3): the job
// detail carries the icon-only raw-log link, and GET /jobs/{id}/raw returns the
// job's full event log as text/plain — one message per line, in sequence order,
// no timestamps or level chrome.
func TestJobRawLogServesPlainText(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: "ansible", Trigger: "manual",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for _, m := range []string{"line one", "line two", "fatal: boom"} {
		if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
			JobID: job.ID, Level: store.LevelInfo, Msg: m,
		}); err != nil {
			t.Fatalf("AppendJobEvent: %v", err)
		}
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	if code != 200 {
		t.Fatalf("job detail status = %d", code)
	}
	for _, want := range []string{
		`id="log-search"`,                // search input present (Q2)
		`/jobs/` + itoa(job.ID) + `/raw`, // raw link target
		`aria-label="Raw log"`,           // the app-bar Raw log glyph action (T6)
	} {
		if !strings.Contains(page, want) {
			t.Errorf("job detail missing %q", want)
		}
	}

	resp, err := client.Get(ts.URL + "/jobs/" + itoa(job.ID) + "/raw")
	if err != nil {
		t.Fatalf("raw get: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("raw status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("raw Content-Type = %q, want text/plain", ct)
	}
	// The three seeded messages each appear once on their own line, in sequence
	// order (the harness driver may append its own events around them, so order
	// is relative, not contiguous).
	i1 := strings.Index(body, "line one")
	i2 := strings.Index(body, "line two")
	i3 := strings.Index(body, "fatal: boom")
	ordered := i1 >= 0 && i1 < i2 && i2 < i3
	if !ordered {
		t.Errorf("raw body must keep seeded messages in relative order, one per line:\n%s", body)
	}
	for _, line := range []string{"line one\n", "line two\n", "fatal: boom\n"} {
		if !strings.Contains(body, line) {
			t.Errorf("raw body missing whole line %q:\n%s", line, body)
		}
	}
}

// TestJobRawLogUnknownIs404: a raw log for a job that does not exist is a 404,
// not an empty or error render.
func TestJobRawLogUnknownIs404(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	resp, err := client.Get(ts.URL + "/jobs/999999/raw")
	if err != nil {
		t.Fatalf("raw get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown job raw status = %d, want 404", resp.StatusCode)
	}
}

// seedJobWithRecap drives a job to a terminal state with a seeded event stream:
// the queue is not told, so the store transitions and events below are the
// whole truth the detail page and list rows read. The job is created via
// CreateGatedJob so it lands in awaiting_approval — a state no harness worker
// claims — and is transitioned to running directly, so a live queue runner can
// never grab the row first (the old CreateJob→queued pattern raced the runner
// and failed under the full -race load: "job N is running, not queued").
func seedJobWithRecap(t *testing.T, h *harness, state store.JobState, code int, recap, fatals []string, args store.JobArgs) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := h.store.CreateGatedJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual", Args: args,
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateAwaitingApproval, store.StateRunning); err != nil {
		t.Fatalf("start seeded job: %v", err)
	}
	for _, l := range fatals {
		if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{JobID: job.ID, Level: store.LevelError, Msg: l}); err != nil {
			t.Fatalf("append fatal: %v", err)
		}
	}
	for _, l := range recap {
		if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{JobID: job.ID, Level: store.LevelInfo, Msg: l}); err != nil {
			t.Fatalf("append recap: %v", err)
		}
	}
	n := 0
	for _, l := range recap {
		if strings.Contains(l, "unreachable=1") {
			n++
		}
	}
	var band string
	switch state {
	case store.StateDegraded:
		band = fmt.Sprintf("degraded (exit %d, %d host(s) unreachable, tolerated)", code, n)
	case store.StateSucceeded:
		band = fmt.Sprintf("succeeded (exit %d)", code)
	default:
		band = fmt.Sprintf("failed (exit %d)", code)
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{JobID: job.ID, Level: store.LevelSystem, Msg: band}); err != nil {
		t.Fatalf("append band: %v", err)
	}
	if err := h.store.SetJobExitCode(ctx, job.ID, code); err != nil {
		t.Fatalf("set exit code: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, state); err != nil {
		t.Fatalf("finish seeded job: %v", err)
	}
	return job
}

func seedFailedWithRecap(t *testing.T, h *harness, code int, recap []string) store.Job {
	t.Helper()
	return seedJobWithRecap(t, h, store.StateFailed, code, recap, []string{
		"fatal: [hypnotoad]: UNREACHABLE! => ssh: connect to host 10.0.20.11 port 22: No route to host",
		"fatal: [lrrr]: UNREACHABLE! => ssh: connect to host 10.0.20.12 port 22: No route to host",
	}, store.JobArgs{Argv: []string{"/bin/true", "--check", "deploy/site.yml"}})
}

func seedDegradedWithRecap(t *testing.T, h *harness, code int, recap []string) store.Job {
	t.Helper()
	return seedJobWithRecap(t, h, store.StateDegraded, code, recap, []string{
		"fatal: [hypnotoad]: UNREACHABLE! => ssh: connect to host 10.0.20.11 port 22: No route to host",
		"fatal: [lrrr]: UNREACHABLE! => ssh: connect to host 10.0.20.12 port 22: No route to host",
	}, store.JobArgs{Argv: []string{"/bin/true", "--check", "deploy/site.yml"}, TolerateUnreachable: true})
}

// TestJobDetailFailedRendersVerdictBand: a FAILED run's detail page renders
// the verdict band with the keyword line, the per-host table (unreachable rows
// state-coloured, the OK host with its recap counts), and the 1a rerun-tolerating
// verb with its meta — the facts the old page buried at the bottom of the tail.
func TestJobDetailFailedRendersVerdictBand(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
		"lrrr : ok=0 changed=0 unreachable=1 failed=0",
		"nibbler : ok=313 changed=4 unreachable=0 failed=0 skipped=130",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`class="verdict-band mt14 verdict-band--failed"`,
		`FAILED — 2 OF 3 HOSTS UNREACHABLE`,
		`Ansible could not reach hypnotoad and lrrr; every play on nibbler completed.`,
		`<span class="result fail"><span class="dot"></span>UNREACHABLE</span>`,
		`<span class="result ok"><span class="dot"></span>OK</span>`,
		`ssh: connect to host 10.0.20.11 port 22: No route to host`,
		`ok=313 changed=4 skipped=130`,
		`Rerun tolerating unreachable hosts →`,
		`settles DEGRADED when the reachable hosts succeed · same as exocomp job rerun ` + itoa64(job.ID) + ` --tolerate-unreachable`,
		`<div class="verdict">exit 4 </div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failed detail missing %q", want)
		}
	}
	if strings.Contains(body, `<span class="tolerated">tolerated</span>`) {
		t.Error("a FAILED run must not render the tolerated verdict tail")
	}
	// The terminal band re-renders from copy (2.3) and repeats completion with
	// the rendered line count: 2 fatals + 4 recap + 1 band = 7 events.
	if !strings.Contains(body, `▸ failed — exit 4 · 7 lines · log complete`) {
		t.Error("failed detail missing the copy-formatted completion band")
	}
}

// TestJobDetailVerdictCompaction: the verdict band stops restating what the run
// card and the host RESULT word already say. The keyword drops the exit figure
// (the run card's .verdict owns it), the RECAP cell drops the zero counts (the
// RESULT word states them), and the settle note rides the verb's own line.
func TestJobDetailVerdictCompaction(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
		"lrrr : ok=0 changed=0 unreachable=1 failed=0",
		"nibbler : ok=313 changed=4 unreachable=0 failed=0 skipped=130",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))

	// The run card's verdict figure carries the exit code; the band must not
	// restate it in the keyword.
	if !strings.Contains(body, `<div class="verdict">exit 4 </div>`) {
		t.Error("the run card must carry the exit figure the band dropped")
	}
	const kwOpen = `<span class="keyword">`
	ki := strings.Index(body, kwOpen)
	if ki < 0 {
		t.Fatal("the verdict band keyword is missing")
	}
	kend := strings.Index(body[ki:], "</span>")
	keyword := body[ki+len(kwOpen) : ki+kend]
	if strings.Contains(keyword, "EXIT") {
		t.Errorf("the band keyword %q must not restate the exit code the run card shows", keyword)
	}
	// Zero counts are dropped from the cell; non-zero counts stay.
	if !strings.Contains(body, `<span class="recap">ok=313 changed=4 skipped=130`) {
		t.Error("the RECAP cell must keep the non-zero counts and drop the zeros")
	}
	if strings.Contains(body, `<span class="recap">ok=313 changed=4 unreachable=0`) {
		t.Error("the RECAP cell must drop the zero counts the RESULT word states")
	}
	// The settle note is a sibling of the verb link inside one .verb-line.
	const lineOpen = `<div class="verb-line">`
	li := strings.Index(body, lineOpen)
	if li < 0 {
		t.Fatal("the band's .verb-line is missing")
	}
	lend := strings.Index(body[li:], "</div>")
	line := body[li : li+lend]
	if !strings.Contains(line, `Rerun tolerating unreachable hosts →`) {
		t.Errorf("the verb must sit in the .verb-line:\n%s", line)
	}
	if !strings.Contains(line, `<span class="meta mono">settles DEGRADED`) {
		t.Errorf("the settle note must ride the verb's line, not a second row:\n%s", line)
	}

	// The denser desktop row and the verb-line layout are pinned in app.css.
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		"height: 32px; padding: 0 12px; font: var(--type-log);",
		".verdict-band .bandverbs .verb-line { display: flex; flex-wrap: wrap; align-items: baseline; gap: 4px 12px; width: 100%; }",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the verdict compaction rule %q", want)
		}
	}
}

// TestDropZeroCounts: the recap filter keeps non-zero counts, drops every
// `key=0`, and returns an all-zero run unchanged so the cell is never blank.
func TestDropZeroCounts(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ok=313 changed=4 unreachable=0 failed=0 skipped=130", "ok=313 changed=4 skipped=130"},
		{"ok=1 changed=0 unreachable=0 failed=0 skipped=0", "ok=1"},
		{"ok=10 changed=100 unreachable=0 failed=0", "ok=10 changed=100"},
		{"ok=0 changed=0 unreachable=0 failed=0 skipped=0", "ok=0 changed=0 unreachable=0 failed=0 skipped=0"},
		{"", ""},
	} {
		if got := dropZeroCounts(tc.in); got != tc.want {
			t.Errorf("dropZeroCounts(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestJobDetailDegradedRendersVerdictBand: a DEGRADED run renders the wait
// band, the wait-coloured verdict tail, the degraded host-count chip, and the
// 1b verbs — never a failure reading.
func TestJobDetailDegradedRendersVerdictBand(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedDegradedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
		"lrrr : ok=0 changed=0 unreachable=1 failed=0",
		"nibbler : ok=313 changed=4 unreachable=0 failed=0 skipped=130",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`class="verdict-band mt14 verdict-band--degraded"`,
		`DEGRADED — SUCCEEDED WITH WARNINGS · 2 OF 3 HOSTS UNREACHABLE`,
		`<span class="chip chip--degraded"><span class="dot"></span>2 hosts unreachable · hypnotoad, lrrr</span>`,
		`<div class="verdict">exit 4 <span class="tolerated">tolerated</span></div>`,
		`<span class="result wait"><span class="dot"></span>UNREACHABLE</span>`,
		`tolerated · ssh: connect to host 10.0.20.11 port 22: No route to host`,
		`Every play on nibbler completed; hypnotoad and lrrr were skipped as unreachable because this run tolerates offline hosts. They have not been configured.`,
		`Rerun limited to hypnotoad, lrrr →`,
		`Open checks — hypnotoad, lrrr DOWN since`,
		`on · settles degraded`,
		`class="tile is-cause wait"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("degraded detail missing %q", want)
		}
	}
	if strings.Contains(body, `FAILED —`) {
		t.Error("a DEGRADED run must not render the FAILED keyword")
	}
	// The degraded band re-renders from copy (2.3) and repeats completion with the
	// rendered line count: 2 fatals + 4 recap + 1 band = 7 events.
	if !strings.Contains(body, `▸ degraded — exit 4 tolerated · 2 hosts unreachable (hypnotoad, lrrr) · reachable hosts succeeded · 7 lines · log complete`) {
		t.Error("degraded detail missing the copy-formatted completion band")
	}
}

// TestJobDetailOutputCardRetune is J6 + §2/§3: the Output bar leads with
// Output · pin · meta then the controls (filter · wrap · Raw ↗); a terminal
// page's pin is server-rendered complete (never following); the static ENDED
// pill is gone; the meta reads "stdout + stderr · ended {hh:mm:ss}"; the last
// log line repeats completion; the Raw ↗ verb sits after wrap.
func TestJobDetailOutputCardRetune(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
	})
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`<div class="output-bar output-head">`,
		`<span class="title">Output</span>`,
		`id="log-pin" data-state="complete"`,
		`<span class="pinword">LOG COMPLETE</span><span class="n"> · 5 lines</span>`,
		`<span class="dot" aria-hidden="true" hidden></span>`,
		`<svg class="check" width="13" height="13"`,
		`<span class="meta mono">stdout + stderr · ended `,
		`<input id="log-search" type="search" autocomplete="off" placeholder="filter log" aria-label="Filter log">`,
		`class="logwrap wrap" id="logwrap"`,
		`<a class="verb-raw" href="/jobs/` + itoa64(job.ID) + `/raw" target="_blank" title="Raw log — plain text, opens in a new tab">`,
		`<span class="word">Raw</span><span class="ext">↗</span>`,
		`▸ failed — exit 4 · 5 lines · log complete`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output card missing %q", want)
		}
	}
	if strings.Contains(body, "Search log") {
		t.Error("the search control must be placeholder-only (J6)")
	}
	// The old ENDED vocabulary retires here (lane D retires the CSS).
	for _, gone := range []string{"ENDED · nothing to follow", `class="pill ended mono"`, `data-state="following"`} {
		if strings.Contains(body, gone) {
			t.Errorf("output card still carries the retired %q", gone)
		}
	}
	// The Raw ↗ verb sits after the wrap toggle, so the verbs never shift.
	wrap := strings.Index(body, `class="logwrap wrap" id="logwrap"`)
	raw := strings.Index(body, `class="verb-raw"`)
	if wrap < 0 || raw <= wrap {
		t.Error("the Raw ↗ verb must sit after the wrap toggle")
	}
	// The output meta's clock is HH:MM:SS.
	if i := strings.Index(body, `stdout + stderr · ended `); i >= 0 {
		clock := body[i+len(`stdout + stderr · ended `):][:8]
		if len(clock) != 8 || clock[2] != ':' || clock[5] != ':' {
			t.Errorf("output meta clock must read HH:MM:SS, got %q", clock)
		}
	}
}

// TestJobsListReasonLines: the two-line list carries the D4 one-fact reason on
// degraded and failed rows — never a host list — and nothing on SUCCEEDED.
func TestJobsListReasonLines(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	seedDegradedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
		"lrrr : ok=0 changed=0 unreachable=1 failed=0",
		"nibbler : ok=1 changed=0 unreachable=0 failed=0 skipped=0",
	})
	seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
		"lrrr : ok=0 changed=0 unreachable=1 failed=0",
	})
	// A syntax-error failure: no recap, the ERROR! banner as the last error.
	// Created gated so no harness worker can claim it before the transitions.
	syntax, err := h.store.CreateGatedJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("seed syntax job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, syntax.ID, store.StateAwaitingApproval, store.StateRunning); err != nil {
		t.Fatalf("start syntax job: %v", err)
	}
	for _, l := range []string{"ERROR! Syntax Error while loading YAML."} {
		if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{JobID: syntax.ID, Level: store.LevelError, Msg: l}); err != nil {
			t.Fatalf("append error: %v", err)
		}
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{JobID: syntax.ID, Level: store.LevelSystem, Msg: "failed (exit 2)"}); err != nil {
		t.Fatalf("append band: %v", err)
	}
	if err := h.store.SetJobExitCode(ctx, syntax.ID, 2); err != nil {
		t.Fatalf("set exit: %v", err)
	}
	if err := h.store.TransitionJob(ctx, syntax.ID, store.StateRunning, store.StateFailed); err != nil {
		t.Fatalf("finish syntax job: %v", err)
	}
	// A clean succeeded run: no reason line.
	if _, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
		Argv: []string{"/bin/true"},
	}); err != nil {
		t.Fatalf("enqueue clean job: %v", err)
	}

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs")
	for _, want := range []string{
		`<span class="reason reason--degraded">2 hosts unreachable</span>`,
		`<span class="reason">exit 4 · 2 hosts unreachable</span>`,
		`<span class="reason">exit 2 · syntax error</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("jobs list missing reason line %q", want)
		}
	}
	// Exactly the three reason lines: SUCCEEDED and the non-terminal rows carry
	// none, and no reason line ever names a host.
	if n := strings.Count(body, `class="reason`); n != 3 {
		t.Errorf("jobs list has %d reason lines, want exactly 3:\n%s", n, body)
	}
}

// TestJobsRowPhoneTwoLine pins the jobs list row's phone form (review P2 / SPEC
// §2.3): line 1 is # · title · chip and line 2 is the flat mono-12 ink-500 meta
// TriggerMeta · Source · Age · Duration in its own .cell--meta node the 719
// block shows. The operator is folded into TriggerMeta — there is no standalone
// operator field on the row. The harness cannot switch viewports, so the guard
// pins the markup the phone CSS consumes.
func TestJobsRowPhoneTwoLine(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// A source-bound, finished run: line 2 carries all four meta pieces.
	job := seedJobWithRecap(t, h, store.StateSucceeded, 0, []string{
		"ok: [node0] => changed=0",
		"PLAY RECAP",
		"node0 : ok=1 changed=0 unreachable=0 failed=0 skipped=0",
	}, []string{}, store.JobArgs{Argv: []string{"/bin/true", "deploy/site.yml"}, Source: "tree"})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs")

	// Line 1: # · title · chip, in the first .line.
	for _, want := range []string{
		`<span class="line"><span class="num">` + itoa64(job.ID) + `</span>`,
		`<b class="mono">deploy/site.yml</b>`,
		`<span class="cell cell--state">`,
		`<span class="cell cell--state"><span class="chip`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("jobs row line 1 missing %q", want)
		}
	}

	// Line 2: the flat mono meta node — TriggerMeta · Source · Age · Duration —
	// with the operator folded into TriggerMeta, never a standalone field.
	metaOpen := `<span class="cell cell--meta"><span class="qmeta">`
	idx := strings.Index(body, metaOpen)
	if idx < 0 {
		t.Fatal("jobs row missing the line-2 .cell--meta qmeta node")
	}
	meta := body[idx+len(metaOpen):]
	if end := strings.Index(meta, "</span>"); end >= 0 {
		meta = meta[:end]
	}
	if !strings.HasPrefix(meta, "cli · operator · tree · ") {
		t.Errorf("line-2 meta must lead TriggerMeta · Source, got %q", meta)
	}
	// Four pieces join with three separators on top of the one inside
	// TriggerMeta itself ("web · operator").
	if n := strings.Count(meta, " · "); n != 4 {
		t.Errorf("line-2 meta must join TriggerMeta · Source · Age · Duration, got %d joins in %q", n, meta)
	}
	if strings.Contains(body, `<span class="operator">`) {
		t.Error("jobs row renders a standalone operator field; it is folded into TriggerMeta")
	}
}

// TestJobsRowPhoneLineOne pins the jobs row's phone line 1 (review P2 /
// integrator browser pass): at 390px line 1 is ONE row — id · title · chip —
// the name ellipsizes and the reason is never part of it (the FAILED chip says
// the state; the exit fact is the line-2 meta's own piece now). The harness
// cannot switch viewports, so the guard pins the phone CSS that makes line 1 a
// single stretching row — the title cell fills and ellipsizes, the chip pins
// right, the desktop state-cell reason drops — plus the reason's move to the
// line-2 meta; the one-line render itself is the integrator's browser pass.
func TestJobsRowPhoneLineOne(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// A failed run with a reason: the guard proves the reason left line 1.
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
		"lrrr : ok=0 changed=0 unreachable=1 failed=0",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs")

	// Line 1 is id · title · chip: the first .line opens with the id, then the
	// title cell, then the chip cell.
	for _, want := range []string{
		`<a class="jobs-row" href="/jobs/` + itoa64(job.ID) + `"><span class="line"><span class="num">` + itoa64(job.ID) + `</span>`,
		`<b class="mono">deploy/site.yml</b>`,
		`<span class="cell cell--state">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("jobs row line 1 missing %q", want)
		}
	}

	// The reason is the line-2 meta's fact now: it reads inside the .cell--meta
	// qmeta, so the phone line 1 never carries the exit text at 390px.
	metaOpen := `<span class="cell cell--meta"><span class="qmeta">`
	idx := strings.Index(body, metaOpen)
	if idx < 0 {
		t.Fatal("jobs row missing the line-2 .cell--meta qmeta node")
	}
	meta := body[idx+len(metaOpen):]
	if end := strings.Index(meta, "</span>"); end >= 0 {
		meta = meta[:end]
	}
	if !strings.Contains(meta, "exit 4 · 2 hosts unreachable") {
		t.Errorf("line-2 meta must carry the reason the review moved off line 1, got %q", meta)
	}

	// The phone block makes line 1 one stretching row: title cell fills and
	// ellipsizes, chip pins right, the desktop state-cell reason drops.
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.jobs-row:not(.queue-row) .line:first-child { justify-content: flex-start; }`,
		`.jobs-row:not(.queue-row) .line:first-child .num::after { content: " ·"; }`,
		`.jobs-row:not(.queue-row) .line:first-child .cell { flex: 0 1 auto; min-width: 0; }`,
		`.jobs-row:not(.queue-row) .line:first-child .cell b { white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }`,
		`.jobs-row:not(.queue-row) .line:first-child .cell--state { flex: none; margin-left: auto; }`,
		`.jobs-row:not(.queue-row) .line:first-child .cell--state .reason { display: none; }`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the phone line-1 rule %q", want)
		}
	}
}

// TestJobDetailFlashCleared is J3 + the flash-posture change: the write routes
// flash to /jobs/{id} via a server-set cookie, the job's own page drops the
// banner (handleJobDetail clears it at the render seam), and a ?flash= URL
// parameter — the old, injectable transport — is ignored outright.
func TestJobDetailFlashCleared(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "playbook", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	// A ?flash= parameter must not render anything (it no longer names a
	// message — reads come from the cookie only).
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID)+"?flash=job+4+enqueued")
	if strings.Contains(body, `class="flash"`) {
		t.Error("a ?flash= URL parameter must not render a flash banner")
	}

	// A real flash cookie (set the way redirectFlash does) on the detail page
	// is also dropped — the rerun fact is the dashed chip, not a banner (J3),
	// and the cookie is consumed so a refresh does not resow it.
	req, _ := http.NewRequest("GET", ts.URL+"/jobs/"+itoa64(job.ID), nil)
	req.AddCookie(&http.Cookie{Name: flashCookie, Value: "job 4 enqueued"})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("detail with flash cookie: %v", err)
	}
	b := readBody(t, resp)
	if strings.Contains(b, "job 4 enqueued") {
		t.Error("job detail must not render the redirected flash cookie banner (J3)")
	}
}

// TestJobDetailSentenceRendersFace: the run card sentence reads the actor and
// the face off the job's ledger — "Queued by laptop from the web" — and a
// job with no actor event (a CLI run) reads "operator from the cli".
func TestJobDetailSentenceRendersFace(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelSystem, Msg: "enqueued by token laptop via web",
		RawJSON: `{"actor_kind":"token","actor_name":"laptop","face":"web"}`,
	}); err != nil {
		t.Fatalf("append actor event: %v", err)
	}

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if !strings.Contains(body, `Queued by laptop from the web · `) {
		t.Errorf("detail sentence missing the recorded face, got:\n%s", body)
	}

	cli, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	_, body = getBody(t, client, ts.URL+"/jobs/"+itoa64(cli.ID))
	if !strings.Contains(body, `Queued by operator from the cli · `) {
		t.Errorf("actorless detail sentence must read operator from the cli, got:\n%s", body)
	}
}

// TestJobDetailRunCardTiles: the RUN CONFIGURATION grid renders the source and
// ref tiles (the ref at the source's current sync, S1), the inventory/limit/
// tags/mode/privileged facts, the secrets binding, and the Tolerate tile.
func TestJobDetailRunCardTiles(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	name := seedSourceCheckout(t, h)
	for _, rs := range []store.RunSecret{
		{Binding: "lab", EnvName: "SSH_KEY", SecretName: "lab-ssh"},
		{Binding: "lab", EnvName: "VAULT", SecretName: "vault-pass"},
	} {
		if _, err := h.store.AddRunSecret(ctx, rs); err != nil {
			t.Fatalf("AddRunSecret: %v", err)
		}
	}
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
		Args: store.JobArgs{
			Argv:   []string{"/bin/true", "--limit", "web", "--tags", "deploy", "--check", "--diff", "deploy/site.yml"},
			Source: name, Inventory: "prod-inv", RunSecretBinding: "lab",
			TolerateUnreachable: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`<span>Source</span> <b><a href="/sources/tree">tree ↗</a></b>`,
		`<span>Ref</span> <b><a href="/sources/tree">main @ 0123456789ab</a></b>`,
		`<span>Inventory</span> <b>prod-inv</b>`,
		`<span>Limit</span> <b>web</b>`,
		`<span>Tags</span> <b>deploy</b>`,
		`<span>Mode</span> <b>check + diff</b>`,
		`<span>Secrets bound</span> <b>lab · 1 more</b>`,
		`class="tile wait"><span>Tolerate unreachable</span> <b>on · settles degraded</b>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run card missing tile %q", want)
		}
	}
}

// TestStateClassDegradedIsWait is S7: the degraded state reads as a wait
// everywhere the chip ladder renders, never a failure or a dim default.
func TestStateClassDegradedIsWait(t *testing.T) {
	if got := stateClass(store.StateDegraded); got != "wait" {
		t.Errorf("stateClass(degraded) = %q, want wait", got)
	}
	if got := stateClass(store.StateFailed); got != "fail" {
		t.Errorf("stateClass(failed) = %q, want fail", got)
	}
	if got := stateClass(store.StateSucceeded); got != "ok" {
		t.Errorf("stateClass(succeeded) = %q, want ok", got)
	}
}

// seedStartedJob creates a job and moves it to running so a test can append its
// own event stream without the real driver racing it. Created via
// CreateGatedJob so it lands in awaiting_approval — a state no harness worker
// claims — then transitioned directly, so the runner can never grab the queued
// row first (the CreateJob->queued pattern raced it under parallel load).
func seedStartedJob(t *testing.T, h *harness, runbook string, trigger string, args store.JobArgs) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := h.store.CreateGatedJob(ctx, store.NewJob{
		Runbook: runbook, Kind: executor.KindAnsible, Trigger: trigger, Args: args,
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateAwaitingApproval, store.StateRunning); err != nil {
		t.Fatalf("start seeded job: %v", err)
	}
	return job
}

// TestJobDetailRunningBandComposesRerunCopy is the running-band copy: a rerun's
// running band composes "▸ running — rerun of #{id} · tolerate unreachable on"
// from the child's ledger (the workspace-lock fact is a wave-1 gap, omitted),
// while a non-rerun keeps the queue's own running line.
func TestJobDetailRunningBandComposesRerunCopy(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	parent := seedStartedJob(t, h, "playbook", "manual", store.JobArgs{Argv: []string{"/bin/true"}})

	child := seedStartedJob(t, h, "playbook", rerunTrigger, store.JobArgs{
		Argv: []string{"/bin/true", "deploy/site.yml"}, TolerateUnreachable: true,
	})
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: child.ID, Level: store.LevelSystem, Msg: "rerun of job " + itoa64(parent.ID),
		RawJSON: fmt.Sprintf(`{"rerun_of":%d}`, parent.ID),
	}); err != nil {
		t.Fatalf("append rerun-of: %v", err)
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: child.ID, Level: store.LevelSystem, Msg: "running deploy/site.yml --check",
	}); err != nil {
		t.Fatalf("append running: %v", err)
	}

	plain := seedStartedJob(t, h, "playbook", "manual", store.JobArgs{Argv: []string{"/bin/true"}})
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: plain.ID, Level: store.LevelSystem, Msg: "running deploy/other.yml",
	}); err != nil {
		t.Fatalf("append plain running: %v", err)
	}

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, childBody := getBody(t, client, ts.URL+"/jobs/"+itoa64(child.ID))
	if !strings.Contains(childBody, `▸ running — rerun of #`+itoa64(parent.ID)+` · tolerate unreachable on`) {
		t.Errorf("rerun running band missing the copy composition:\n%s", childBody)
	}
	_, plainBody := getBody(t, client, ts.URL+"/jobs/"+itoa64(plain.ID))
	if !strings.Contains(plainBody, `▸ running — deploy/other.yml`) {
		t.Errorf("non-rerun running band must keep the queue's own line:\n%s", plainBody)
	}
}

// TestJobsListScheduleTriggerMeta: a schedule-fired row's TRIGGER meta is
// "{schedule name} · {fire time}" per SCREENS 2a — not a duplicate of the
// playbook cell. The fire time is the job's own queued instant (the scheduler
// enqueues at the fire).
func TestJobsListScheduleTriggerMeta(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly", CronExpr: "0 2 * * *", Kind: store.ScheduleShell,
	}); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	// A schedule-fired ansible play: the playbook cell is the playbook path and
	// the trigger meta is the schedule name + fire time.
	seedStartedJob(t, h, "nightly", "schedule", store.JobArgs{Argv: []string{"/bin/true", "deploy/site.yml"}})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs")
	if !strings.Contains(body, `<b>Schedule</b>`) {
		t.Errorf("schedule row missing the Schedule trigger label:\n%s", body)
	}
	if !strings.Contains(body, `<small class="mono">nightly · `) {
		t.Errorf("schedule row missing the name + fire time trigger meta:\n%s", body)
	}
	if strings.Contains(body, `<small class="mono">nightly</small>`) {
		t.Error("schedule trigger meta must not duplicate the bare schedule name without a time")
	}
}

// TestJobsListManualTriggerMetaCarriesFace: a token-attributed manual run's
// TRIGGER meta carries the recorded face ("web · operator"), and a CLI run with
// no actor event reads "cli · operator".
func TestJobsListManualTriggerMetaCarriesFace(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	web := seedStartedJob(t, h, "playbook", "manual", store.JobArgs{Argv: []string{"/bin/true"}})
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: web.ID, Level: store.LevelSystem, Msg: "enqueued by token laptop via web",
		RawJSON: `{"actor_kind":"token","actor_name":"laptop","face":"web"}`,
	}); err != nil {
		t.Fatalf("append actor: %v", err)
	}
	seedStartedJob(t, h, "playbook", "manual", store.JobArgs{Argv: []string{"/bin/true"}})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs")
	if !strings.Contains(body, `<small class="mono">web · operator</small>`) {
		t.Errorf("manual row missing the web face:\n%s", body)
	}
	if !strings.Contains(body, `<small class="mono">cli · operator</small>`) {
		t.Errorf("actorless manual row missing the cli fallback:\n%s", body)
	}
}

// TestJobsListAppbarCount is R4's count move: the jobs count + "newest first"
// ordering fact live in the app bar meta, and the list's own count span is
// dropped from the markup.
func TestJobsListAppbarCount(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedStartedJob(t, h, "playbook", "manual", store.JobArgs{Argv: []string{"/bin/true"}})
	seedStartedJob(t, h, "playbook", "manual", store.JobArgs{Argv: []string{"/bin/true"}})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs")
	if !strings.Contains(body, `<span class="appbar-meta">2 · newest first</span>`) {
		t.Errorf("jobs app bar missing the count meta:\n%s", body)
	}
	if strings.Contains(body, `<span class="mono">2 · newest first</span>`) {
		t.Error("the jobs list must not render its own count span (the count moved to the app bar)")
	}
}

// TestJobsListHeaderOrderMatchesRows pins the .jobs-head column labels to the
// row DOM's column order: the stacked row is # · playbook · STATE · trigger ·
// ran (the state cell sits on line 1, before the trigger on line 2), so the
// desktop re-flow must label column 3 State and column 4 Trigger. A header
// that swaps them would mislabel the desktop grid.
func TestJobsListHeaderOrderMatchesRows(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs")
	if !strings.Contains(body, `<span>#</span><span>Playbook</span><span>State</span><span>Trigger</span><span>Ran</span>`) {
		t.Errorf("jobs header must read # · Playbook · State · Trigger · Ran:\n%s", body)
	}
	if strings.Contains(body, `<span>#</span><span>Playbook</span><span>Trigger</span><span>State</span><span>Ran</span>`) {
		t.Error("jobs header must not emit the old Trigger-before-State column order")
	}
	// The row DOM agrees: the state chip renders before the trigger label.
	chip := strings.Index(body, `class="chip`)
	trigger := strings.Index(body, `>Manual</b>`)
	if chip < 0 || trigger < 0 || chip > trigger {
		t.Errorf("row must place the state chip before the trigger cell (chip %d, trigger %d)", chip, trigger)
	}
}

// TestJobDetailRunCardOrder: the compact run card stacks chips (with the
// verdict inline) < sentence < tiles in byte order, and never restates the app
// bar's own title / job #. The one meta line carries the queued-by fact and the
// run duration.
func TestJobDetailRunCardOrder(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
		"nibbler : ok=313 changed=4 unreachable=0 failed=0 skipped=130",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	chips := strings.Index(body, `<div class="chips">`)
	verdict := strings.Index(body, `<div class="verdict">`)
	sentence := strings.Index(body, `<p class="sentence">`)
	ordered := chips >= 0 && chips < verdict && verdict < sentence
	if !ordered {
		t.Errorf("run card must stack chips < verdict < sentence (indexes %d %d %d)", chips, verdict, sentence)
	}
	// The app bar carries the title and job #; the card must not restate them.
	for _, gone := range []string{`class="runtitle"`, `class="jobid"`, `class="title mono">`} {
		if strings.Contains(body, gone) {
			t.Errorf("the compact run card must not restate the app bar's title/job # (%q)", gone)
		}
	}
	// The one meta line carries the queued-by fact and the run duration.
	if !strings.Contains(body, `Queued by operator from the cli · `) {
		t.Error("run card meta line missing the queued-by fact")
	}
	if !strings.Contains(body, `<span class="ranline">· took `) {
		t.Errorf("run card meta line missing the inline duration:\n%s", body)
	}
}

// TestJobDetailConfigWraps is R6: the RUN CONFIGURATION tile values wrap on
// phone instead of ellipsizing (a value is never a truncated link), and the
// tablet band widens the grid to two columns.
func TestJobDetailConfigWraps(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if !strings.Contains(body, `<div class="config">`) {
		t.Errorf("run card missing the config grid:\n%s", body)
	}
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.config .tile b, .config .tile a { overflow-wrap: anywhere; white-space: normal; text-overflow: clip; }`,
		`@media (min-width: 720px) and (max-width: 1099px) {`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the config wrap rule %q (R6)", want)
		}
	}
}

// TestJobDetailHostTableSecondLine is R7: each host row is name + result on
// the first line (the .top node) with the recap as a second line, and the
// header carries the RECAP label the phone sheet hides.
func TestJobDetailHostTableSecondLine(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
		"nibbler : ok=313 changed=4 unreachable=0 failed=0 skipped=130",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`<div class="head" aria-hidden="true">`,
		`<span>HOST</span><span>RESULT</span><span class="recap">RECAP</span>`,
		`<div class="row">`,
		`<div class="top">`,
		`<span class="host">hypnotoad</span>`,
		`<span class="result fail"><span class="dot"></span>UNREACHABLE</span>`,
		`<span class="result ok"><span class="dot"></span>OK</span>`,
		`<span class="recap">ssh: connect to host 10.0.20.11 port 22: No route to host</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("host table missing %q", want)
		}
	}
	// The recap is the row's second line: it renders outside the .top node.
	top := strings.Index(body, `<div class="top">`)
	recap := strings.Index(body, `<span class="recap">ssh: connect`)
	if top < 0 || recap <= top {
		t.Errorf("host recap must render after the .top first line:\n%s", body)
	}
}

// TestJobDetailOutputHeadRows is R8/§2: the Output header is one bar whose
// lead is Output · pin · meta (the pin a fixed slot between the title and the
// meta), with the filter · wrap · Raw controls on the right.
func TestJobDetailOutputHeadRows(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`<div class="output-bar output-head">`,
		`<span class="lead">`,
		`<span class="title">Output</span>`,
		`<span class="pin" id="log-pin" data-state="complete"`,
		`<span class="meta mono">stdout + stderr · ended `,
		`<span class="controls">`,
		`<input id="log-search"`,
		`class="logwrap wrap" id="logwrap" aria-pressed="false">wrap</button>`,
		`<span class="word">Raw</span><span class="ext">↗</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output head missing %q", want)
		}
	}
	// Output · pin · meta come before the controls.
	title := strings.Index(body, `<span class="title">Output</span>`)
	pin := strings.Index(body, `id="log-pin"`)
	meta := strings.Index(body, `stdout + stderr · ended `)
	controls := strings.Index(body, `<span class="controls">`)
	if title < 0 || title >= pin || pin >= meta || meta >= controls {
		t.Errorf("output head must lead with Output · pin · meta then controls (indexes %d %d %d %d)", title, pin, meta, controls)
	}
}

// TestJobDetailTailInToolbar is the QA delta for Tail: the Tail +N verb lives
// in the Output toolbar's right group BETWEEN wrap and Raw ↗ — never a
// floating button over the log box. It renders hidden (the JS reveals it when
// the operator scrolls up) and the streaming JS still re-points at the same
// ids, now toolbar-anchored. The .tail-btn rule (position:absolute over the
// log) is retired.
func TestJobDetailTailInToolbar(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	// Terminal page: the verb renders in the bar, hidden, before Raw.
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
	})
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	wrap := strings.Index(body, `class="logwrap wrap" id="logwrap"`)
	tail := strings.Index(body, `class="verb-tail" id="log-tail"`)
	raw := strings.Index(body, `class="verb-raw"`)
	if wrap < 0 || tail < 0 || raw < 0 || tail <= wrap || raw <= tail {
		t.Error("the Tail verb must sit between wrap and Raw in the Output toolbar")
	}
	for _, want := range []string{
		`class="verb-tail" id="log-tail" hidden`,
		`<span class="word">Tail</span><small id="log-tail-n">+0</small>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("toolbar Tail verb missing %q", want)
		}
	}
	if strings.Contains(body, "tail-btn") {
		t.Error("the floating .tail-btn button must be gone from the page")
	}
	// The log box is a plain child of the card — no button inside it.
	logOpen := strings.Index(body, `<div id="log" class="log"`)
	logClose := strings.Index(body[logOpen:], "</div>")
	if logClose >= 0 && strings.Contains(body[logOpen:logOpen+logClose], "verb-tail") {
		t.Error("the Tail verb must not render inside the log box")
	}

	// Live page: the streaming JS re-points at the toolbar verb by id.
	live := seedStartedJob(t, h, "playbook", "manual", store.JobArgs{Argv: []string{"/bin/true"}})
	_, liveBody := getBody(t, client, ts.URL+"/jobs/"+itoa64(live.ID))
	if !strings.Contains(liveBody, `id="log-pin" data-state="following"`) {
		t.Error("the live page must render the pin following")
	}
	if !strings.Contains(liveBody, `var tailBtn = document.getElementById("log-tail")`) {
		t.Error("the streaming JS must re-point at the toolbar Tail verb")
	}

	// CSS: the Tail verb is a 32px labelled toolbar verb; the absolute
	// floating rule and the relative log wrapper are retired.
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	if !strings.Contains(app, ".verb-tail {") {
		t.Error("app.css missing the .verb-tail toolbar verb")
	}
	if strings.Contains(app, ".tail-btn") {
		t.Error("app.css still carries the floating .tail-btn rule")
	}
	if strings.Contains(app, ".logwrap2 { position: relative") {
		t.Error("the log wrapper must not anchor a floating button")
	}
	// Phone keeps it usable: the word drops, the glyph and the +N stay.
	if !strings.Contains(app, ".verb-tail .word { display: none; }") {
		t.Error("the phone block must drop the Tail word (glyph + N)")
	}
}

// TestJobDetailPinStates is the pin's server-set data-state (§2): complete on
// a terminal page whose event list carries a terminal band, cut on a terminal
// page without one (an interrupted runner), following on a live page — and a
// terminal page never reads following.
func TestJobDetailPinStates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	// Terminal with a terminal band -> complete (the check glyph replaces the
	// dot, so the pin is data-backed, not JS).
	failed := seedFailedWithRecap(t, h, 4, []string{"PLAY RECAP"})
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(failed.ID))
	if !strings.Contains(body, `id="log-pin" data-state="complete"`) {
		t.Error("a terminal job with a terminal band must render the pin complete")
	}
	if strings.Contains(body, `data-state="following"`) {
		t.Error("a terminal page must never render the pin following")
	}

	// Terminal without a terminal band (cancelled mid-run, no verdict line)
	// -> cut.
	cut, err := h.store.CreateGatedJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("seed cut job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, cut.ID, store.StateAwaitingApproval, store.StateRunning); err != nil {
		t.Fatalf("start cut job: %v", err)
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{JobID: cut.ID, Level: store.LevelInfo, Msg: "ansible-playbook 2.17.4"}); err != nil {
		t.Fatalf("append line: %v", err)
	}
	if err := h.store.TransitionJob(ctx, cut.ID, store.StateRunning, store.StateCancelled); err != nil {
		t.Fatalf("cancel cut job: %v", err)
	}
	_, body = getBody(t, client, ts.URL+"/jobs/"+itoa64(cut.ID))
	if !strings.Contains(body, `id="log-pin" data-state="cut"`) {
		t.Error("a terminal job without a terminal band must render the pin cut")
	}
	if !strings.Contains(body, `<span class="pinword">LOG CUT</span><span class="n"> · 1 lines</span>`) {
		t.Error("the cut pin must read LOG CUT with the rendered line count")
	}
	if !strings.Contains(body, `<span class="dot" aria-hidden="true"></span>`) {
		t.Error("the cut pin must show the dot, not the check glyph")
	}

	// Live -> following.
	live := seedStartedJob(t, h, "playbook", "manual", store.JobArgs{Argv: []string{"/bin/true"}})
	_, body = getBody(t, client, ts.URL+"/jobs/"+itoa64(live.ID))
	if !strings.Contains(body, `id="log-pin" data-state="following"`) {
		t.Error("a live job must render the pin following")
	}
	if !strings.Contains(body, `<span class="pinword">FOLLOWING TAIL</span>`) {
		t.Error("the live pin must read FOLLOWING TAIL")
	}
	if !strings.Contains(body, `stdout + stderr · 0 lines · streaming`) {
		t.Error("the live output meta must read 'N lines · streaming'")
	}
}

// TestJobDetailSucceededBand is the succeeded terminal band (§2): the queue's
// "succeeded (exit N)" system line re-renders as the ok-tint band, the last
// log line repeats completion with the rendered line count, and the pin reads
// complete with the ended meta.
func TestJobDetailSucceededBand(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedJobWithRecap(t, h, store.StateSucceeded, 0, []string{
		"ok: [node0] => changed=0",
		"PLAY RECAP",
		"node0 : ok=1 changed=0 unreachable=0 failed=0 skipped=0",
	}, []string{}, store.JobArgs{Argv: []string{"/bin/true"}})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`class="logband ok">`,
		`▸ succeeded — exit 0 · 4 lines · log complete`,
		`id="log-pin" data-state="complete"`,
		`stdout + stderr · ended `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("succeeded detail missing %q", want)
		}
	}
}

// TestJobDetailLogPreLines is R9: the log is a .line-per-row div (not a <pre>),
// each row a ts/msg grid with the HH:MM:SS clock column, and the wrap toggle
// flips the .log.wrap class. The band rows stay full-width.
func TestJobDetailLogPreLines(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`<div id="log" class="log" role="log" aria-live="polite" aria-label="Job log">`,
		`<span class="line"><span class="ts">`,
		`<span class="ts">`,
		`<span class="msg">`,
		`class="logband fail"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("log missing %q", want)
		}
	}
	if strings.Contains(body, `<pre id="log"`) {
		t.Error("the log must be a .line div, not a <pre> (R9)")
	}
	// The clocks are HH:MM:SS (R9): the date lives on the card.
	if i := strings.Index(body, `<span class="ts">`); i >= 0 {
		clock := body[i+len(`<span class="ts">`):][:8]
		if len(clock) != 8 || clock[2] != ':' || clock[5] != ':' {
			t.Errorf("log clocks must read HH:MM:SS, got %q:\n%s", clock, body)
		}
	} else {
		t.Error("log has no clock row at all")
	}
}

// TestJobDetailSelfCheckPhone is review P3: the doctor self-check rows stack
// on phone — name · chip on line 1, the detail full width on line 2 wrapping
// anywhere (the detail cell clips http://127.0.0.1:39099/me… at its 130px
// desktop column). The harness cannot switch viewports, so the guard pins the
// phone block's .doctor-findings rules in app.css.
func TestJobDetailSelfCheckPhone(t *testing.T) {
	h := newHarness(t)
	job := seedDoctorJob(t, h,
		findingSeed{name: "data_dir", status: "ok", detail: "mode 700 — writable"},
		findingSeed{name: "endpoint", status: "fail", detail: "http://127.0.0.1:39099/me… unreachable"})
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`class="card mt14 doctor-findings"`,
		`<div class="finding-rows">`,
		`<span class="mono">endpoint</span>`,
		`<span class="detail">http://127.0.0.1:39099/me… unreachable</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("self-check card missing %q", want)
		}
	}
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.doctor-findings .finding-rows .row { display: flex; flex-direction: column;`,
		`.doctor-findings .finding-rows .row .detail { overflow-wrap: anywhere;`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the phone self-check rule %q (P3)", want)
		}
	}
}

// TestJobDetailVerdictPhone is review P4: the run card's head is the chip row
// with the verdict figure, and on phone the head stacks — the chips keep the
// full width and the exit figure sits on its own line below (the old
// .verdict-wrap right-align left a 200px void beside the figure). The harness
// cannot switch viewports, so the guard pins the phone block's .run-card rules
// in app.css.
func TestJobDetailVerdictPhone(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
		"nibbler : ok=313 changed=4 unreachable=0 failed=0 skipped=130",
	})
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`<div class="run-head">`,
		`<div class="chips">`,
		`<div class="verdict">exit 4 `,
		`<p class="sentence">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run card verdict area missing %q", want)
		}
	}
	if strings.Contains(body, `class="verdict-wrap"`) || strings.Contains(body, `class="runtitle"`) {
		t.Error("the run card must not keep the retired verdict-wrap / runtitle nodes")
	}
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.run-card .run-head { flex-direction: column; align-items: stretch; gap: 8px; }`,
		`.run-card .verdict { font-size: 28px;`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the phone verdict left-stack rule %q (P4)", want)
		}
	}
}

// TestJobDetailRunCardChipsWrapPhone: on phone the run-card chip row wraps a
// long chip (the degraded host list) instead of scrolling it behind a clipped
// edge — a value is never truncated on phone. The harness cannot switch
// viewports, so the guard pins the markup and the phone block's rules.
func TestJobDetailRunCardChipsWrapPhone(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedDegradedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
		"lrrr : ok=0 changed=0 unreachable=1 failed=0",
		"nibbler : ok=1 changed=0 unreachable=0 failed=0 skipped=0",
	})
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if !strings.Contains(body, `class="chip chip--degraded"><span class="dot"></span>2 hosts unreachable · hypnotoad, lrrr</span>`) {
		t.Errorf("degraded run card missing the host-count chip:\n%s", body)
	}
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.run-card .chips { overflow-x: visible; }`,
		`.run-card .chips .chip { height: auto; min-height: 36px; max-width: 100%; white-space: normal; overflow-wrap: anywhere; }`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the phone run-card chip-wrap rule %q", want)
		}
	}
}

// TestJobDetailOutputPhone is review P5: the output header is a two-row bar on
// phone, the wrap toggle is wired to a real .log.wrap rule (horizontal scroll)
// instead of being a no-op against the pre-wrap default, and the pane is
// bounded by max-height rather than a fixed viewport height. The harness cannot
// switch viewports, so the guard pins the markup and the phone block's rules.
func TestJobDetailOutputPhone(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedFailedWithRecap(t, h, 4, []string{
		"PLAY RECAP",
		"hypnotoad : ok=0 changed=0 unreachable=1 failed=0",
	})
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`<div class="output-bar output-head">`,
		`<span class="lead">`,
		`<span class="controls">`,
		`class="logwrap wrap" id="logwrap" aria-pressed="false">wrap</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output head missing %q", want)
		}
	}
	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		`.log.wrap { white-space: pre;`,
		`.output-head { flex-direction: column;`,
		`.output-head .wrap { height: 36px;`,
		`.logwrap2 > #log { min-height: 240px; max-height: 60vh; }`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the phone output rule %q (P5)", want)
		}
	}
}

// TestJobDetailOutputWrapDefaultPointerIndependent is review R5's correction:
// the round-2 capture read raw because it was fine-pointer; the 719 block now
// wraps by default for every pointer, so the wrap default no longer depends on
// a coarse-pointer media query. The .wrap toggle is the raw state. The MUTATION
// — restoring the 719 raw pre — reds this test.
func TestJobDetailOutputWrapDefaultPointerIndependent(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	if !strings.Contains(app, ".log{max-height:60vh;white-space:pre-wrap; overflow:auto; font-size:12px; }") {
		t.Error("the 719 log block must wrap by default (pre-wrap), independent of pointer")
	}
	if strings.Contains(app, ".log{max-height:60vh;white-space:pre;") {
		t.Error("the 719 log block must not keep the raw pre default")
	}
	if !strings.Contains(app, ".log .line { grid-template-columns: 60px 1fr; width: auto;") {
		t.Error("the 719 log line must wrap (width:auto), not hold max-content")
	}
	if !strings.Contains(app, ".log.wrap .line { white-space: pre; width: max-content; }") {
		t.Error("the .wrap toggle must be the raw state (pre, max-content)")
	}
	// The keys-hint coarse rule stays; the now-redundant coarse wrap block does
	// not (a coarse phone and a fine-pointer phone read the same 719 default).
	if !strings.Contains(app, "@media (pointer: coarse) { .keys-hint { display: none; } }") {
		t.Error("the keys-hint coarse rule must stay")
	}
	if strings.Contains(app, "@media (pointer: coarse) {\n  .log { white-space: pre-wrap; }") {
		t.Error("the redundant coarse wrap block must be gone; the 719 default is enough")
	}
}

// TestJobDetailHostRecapWraps is the m108 gate for the verdict band's HOST
// table RECAP. Desktop: the base rule ellipsizes the recap, which needs a
// shrinkable grid track — a bare `1fr` keeps `min-width: auto`, so the column
// widens to the nowrap content and the ellipsis never fires. Phone: the row
// stacks and the recap owns a full-width line 2, so the 719 block must reset
// the base `white-space: nowrap` or the long recap overflows the row. The
// mutations — reverting either grid to `1fr`, or dropping the 719
// `white-space: normal` reset — red this test.
func TestJobDetailHostRecapWraps(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, app := getBody(t, client, ts.URL+"/assets/app.css")

	if got := strings.Count(app, "grid-template-columns: 150px 120px minmax(0, 1fr);"); got != 2 {
		t.Errorf("host-table head+row grids must both use minmax(0, 1fr), found %d", got)
	}
	if strings.Contains(app, "grid-template-columns: 150px 120px 1fr;") {
		t.Error("host-table grid still uses a bare 1fr (min-width:auto overflows the recap)")
	}
	if !strings.Contains(app, ".host-table .row .recap { color: var(--ink-500); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }") {
		t.Error("the desktop recap must keep nowrap+ellipsis")
	}
	if !strings.Contains(app, ".host-table .row .recap { font-size: 11.5px; white-space: normal; overflow: visible; text-overflow: clip; overflow-wrap: anywhere; }") {
		t.Error("the 719 recap rule must reset nowrap so the full-width line wraps")
	}
}

// TestJobRerunDialogBareVerbWithMetaContract: the rerun dialog's submit is the
// bare verb — the thing being queued (title, and @ ref when present) lives on
// the dialog's meta line, the same split the launch drawer holds to, so a long
// path can never wrap the primary.
func TestJobRerunDialogBareVerbWithMetaContract(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	job := seedTerminalJob(t, h, store.JobArgs{Argv: []string{"/bin/true"}, Source: name})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if !strings.Contains(body, `>Queue run</button>`) {
		t.Error("rerun dialog submit must be the bare verb Queue run")
	}
	if strings.Contains(body, "Queue run —") {
		t.Error("rerun dialog submit must not restate the queued run")
	}
	if !strings.Contains(body, "re-queues /bin/true @ main") {
		t.Error("rerun dialog meta must name the queued title and ref")
	}
}
