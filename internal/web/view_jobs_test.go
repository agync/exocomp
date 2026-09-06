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
		`id="log-search"`,                                // search input present (Q2)
		`/jobs/` + itoa(job.ID) + `/raw`,                 // raw link target
		`target="_blank" rel="noopener"`,                 // new tab
		`aria-label="View raw log in new tab"`,           // icon-only, named
		`<path d="M14 4h6v6M10 14l10-10M19 13v7H4V5h7">`, // the open glyph
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
