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

// TestJobDetailBreadcrumb: the job detail view carries a breadcrumb trail back to
// the jobs list, ending in the current job (unlinked, aria-current).
func TestJobDetailBreadcrumb(t *testing.T) {
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
	for _, want := range []string{
		`class="breadcrumb"`,
		`href="/jobs">Jobs</a>`,
		`aria-current="page">#` + itoa(job.ID) + `</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("job detail missing breadcrumb %q", want)
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
		`new EventSource("/jobs/" + job + "/stream?since=" + since)`, // opens the tail with the cursor
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
		`>Raw log</a>`,                   // the app-bar [Raw log] action
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
	band := fmt.Sprintf("failed (exit %d)", code)
	if state == store.StateDegraded {
		band = fmt.Sprintf("degraded (exit %d, %d host(s) unreachable, tolerated)", code, n)
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
		`FAILED · EXIT 4 — 2 OF 3 HOSTS UNREACHABLE`,
		`Ansible could not reach hypnotoad and lrrr; every play on nibbler completed.`,
		`<span class="result fail"><span class="dot"></span>UNREACHABLE</span>`,
		`<span class="result ok"><span class="dot"></span>OK</span>`,
		`ssh: connect to host 10.0.20.11 port 22: No route to host`,
		`ok=313 changed=4 unreachable=0 failed=0 skipped=130`,
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
	// The terminal band re-renders from copy (2.3).
	if !strings.Contains(body, `▸ failed — exit 4 · unreachable hosts not tolerated · ntfy notified`) {
		t.Error("failed detail missing the copy-formatted failed band")
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
	if strings.Contains(body, `FAILED · EXIT`) {
		t.Error("a DEGRADED run must not render the FAILED keyword")
	}
	// The degraded band re-renders from copy (2.3).
	if !strings.Contains(body, `▸ degraded — exit 4 tolerated · 2 hosts unreachable (hypnotoad, lrrr) · reachable hosts succeeded`) {
		t.Error("degraded detail missing the copy-formatted degraded band")
	}
}

// TestJobDetailOutputCardRetune is J6: the Output header carries the line
// meta and the ENDED pill, the filter input is placeholder-only (no "Search
// log" label), and the wrap toggle is present.
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
		`<div class="outputbar output-head">`,
		`<span class="outputtitle">Output</span>`,
		`<span class="outputmeta mono ml-auto">stdout + stderr · 5 lines · complete</span>`,
		`<span class="pill ended mono">ENDED · nothing to follow</span>`,
		`<input id="log-search" type="search" autocomplete="off" placeholder="filter log" aria-label="Filter log">`,
		`class="logwrap wrap" id="logwrap"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output card missing %q", want)
		}
	}
	if strings.Contains(body, "Search log") {
		t.Error("the search control must be placeholder-only (J6)")
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

// seedStartedJob creates a job and synchronously moves it to running before the
// harness queue's 20ms tick can claim it, so a test can append its own event
// stream without the real driver racing it (the same window seedJobWithRecap
// relies on).
func seedStartedJob(t *testing.T, h *harness, runbook string, trigger string, args store.JobArgs) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: runbook, Kind: executor.KindAnsible, Trigger: trigger, Args: args,
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
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

// TestJobDetailRunCardOrder is R5's card stack: chips, then the title + job #,
// then the verdict number, then the sentence — in byte order, never the old
// title-and-sentence column beside the verdict.
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
	title := strings.Index(body, `class="title mono">`)
	verdict := strings.Index(body, `<div class="verdict">`)
	sentence := strings.Index(body, `<p class="sentence">`)
	ordered := chips >= 0 && chips < title && title < verdict && verdict < sentence
	if !ordered {
		t.Errorf("run card must stack chips < title < verdict < sentence (indexes %d %d %d %d)", chips, title, verdict, sentence)
	}
	// The title and the job # sit together on the title line.
	if !strings.Contains(body, `class="jobid">job #`+itoa64(job.ID)) {
		t.Errorf("run card title must pair with job #%d:\n%s", job.ID, body)
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

// TestJobDetailOutputHeadRows is R8: the Output header is two rows — the title
// with the line meta, then the filter + wrap controls — with ENDED as a plain
// mono line on phone (the pill styling it keeps on desktop).
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
		`<div class="outputbar output-head">`,
		`<span class="controls">`,
		`<span class="outputtitle">Output</span>`,
		`<span class="outputmeta mono ml-auto">stdout + stderr · `,
		`<span class="logtools controls ml-auto">`,
		`<input id="log-search"`,
		`class="logwrap wrap" id="logwrap" aria-pressed="false">wrap</button>`,
		`<span class="pill ended mono">ENDED · nothing to follow</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("output head missing %q", want)
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
		`<span class="line" style="display:grid;grid-template-columns:60px 1fr">`,
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
