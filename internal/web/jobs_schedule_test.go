package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// waitJobTerminal polls until a job reaches any terminal state, for a test
// that only needs the job out of queued/running (unlike waitJobState, which
// pins one exact outcome).
func waitJobTerminal(t *testing.T, h *harness, id int64, within time.Duration) store.Job {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		job, err := h.store.GetJob(context.Background(), id)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if job.State.Terminal() {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d is %s after %s, want terminal", id, job.State, within)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// seedSchedulableJob writes a terminal-shaped ansible job straight into the
// store (bypassing the queue, like render_test.go's seed jobs do) with the
// stored Argv the schedule-from-job path must reproduce.
func seedSchedulableJob(t *testing.T, h *harness, argv []string) store.Job {
	t.Helper()
	job, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
		Args: store.JobArgs{Argv: argv},
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return job
}

func TestJobScheduleReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedSchedulableJob(t, h, []string{"/bin/true"})
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/jobs/"+itoa64(job.ID)+"/schedule",
		url.Values{"name": {"nightly"}, "cron": {"@every 5m"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly job schedule = %d, want 403", resp.StatusCode)
	}
}

func TestJobScheduleCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedSchedulableJob(t, h, []string{"/bin/true"})
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/jobs/"+itoa64(job.ID)+"/schedule",
		url.Values{"name": {"nightly"}, "cron": {"@every 5m"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin job schedule = %d, want 403", resp.StatusCode)
	}
}

func TestJobScheduleCreatesFromStoredJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	argv := []string{"/usr/bin/ansible-playbook", "--inventory", "hosts.ini", "site.yml"}
	job := seedSchedulableJob(t, h, argv)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	// The form carries no argv/kind field at all — the handler must build both
	// from the stored job, not from anything a client could post. A posted
	// "argv"/"kind" field (as if a stale or tampered form) must be ignored.
	resp := postForm(t, client, ts.URL+"/jobs/"+itoa64(job.ID)+"/schedule",
		url.Values{
			"name": {"replay-site"}, "cron": {"@every 6h"},
			"argv": {"rm -rf /"}, "kind": {"backup"},
		}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("job schedule = %d, want 303", resp.StatusCode)
	}

	rows, err := h.store.ListSchedules(ctx, false)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	var found *store.Schedule
	for i := range rows {
		if rows[i].Name == "replay-site" {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("schedule was not created")
	}
	if found.Kind != store.ScheduleShell {
		t.Fatalf("kind = %q, want %q (client-posted kind must be ignored)", found.Kind, store.ScheduleShell)
	}
	if len(found.Args.Argv) != len(argv) {
		t.Fatalf("argv = %v, want %v", found.Args.Argv, argv)
	}
	for i, a := range argv {
		if found.Args.Argv[i] != a {
			t.Fatalf("argv[%d] = %q, want %q (must come from the stored job, not the form)", i, found.Args.Argv[i], a)
		}
	}
}

func TestJobScheduleBogusJobID404(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/jobs/999999/schedule",
		url.Values{"name": {"nightly"}, "cron": {"@every 5m"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bogus job id schedule = %d, want 404", resp.StatusCode)
	}
}

func TestJobScheduleRefusesSourceJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "command", Kind: executor.KindAnsible, Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}, Dir: "/some/checkout"},
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/jobs/"+itoa64(job.ID)+"/schedule",
		url.Values{"name": {"nightly"}, "cron": {"@every 5m"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("source-dir job schedule = %d, want 400", resp.StatusCode)
	}
}

// TestJobDetailShowsScheduleAndRunAgain: a terminal, schedulable ansible job
// renders "Run again" and "Schedule…" for an operator; the "Run again" resubmit
// is a modal trigger (not a source link) even for a job with no source, since an
// ad-hoc job with an argv can rerun too. A readonly viewer sees neither.
func TestJobDetailShowsScheduleAndRunAgain(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedSchedulableJob(t, h, []string{"/bin/true", "ok"})
	h.waitJobState(t, job.ID, store.StateSucceeded, 3*time.Second)

	op := h.mint(t, "laptop", "operator")
	opClient := login(t, ts.URL, op)
	_, body := getBody(t, opClient, ts.URL+"/jobs/"+itoa64(job.ID))
	// The Rerun action moved to the phone app bar (SPEC §2.0): it opens the
	// resubmit modal through the JS-off ?rerun=1 path.
	if !strings.Contains(body, `aria-label="Rerun"`) || !strings.Contains(body, `/jobs/`+itoa64(job.ID)+`?rerun=1`) {
		t.Error("a terminal job with an argv must offer the Rerun resubmit modal from the app bar")
	}
	if !strings.Contains(body, `id="job-rerun"`) {
		t.Error("the rerun modal must be present")
	}
	if !strings.Contains(body, `action="/jobs/`+itoa64(job.ID)+`/rerun"`) {
		t.Error("rerun form must post to this job's own rerun route")
	}
	if !strings.Contains(body, `data-modal-open="job-schedule"`) {
		t.Error("operator's job detail must show a Schedule… trigger")
	}
	if strings.Contains(body, `data-modal-open="job-schedule" disabled`) {
		t.Error("Schedule must be enabled for a succeeded job")
	}
	if !strings.Contains(body, "/bin/true ok") {
		t.Error("schedule modal must preview the job's own argv")
	}
	if !strings.Contains(body, `action="/jobs/`+itoa64(job.ID)+`/schedule"`) {
		t.Error("schedule form must post to this job's own schedule route")
	}

	ro := h.mint(t, "ro", "readonly")
	roClient := login(t, ts.URL, ro)
	_, body = getBody(t, roClient, ts.URL+"/jobs/"+itoa64(job.ID))
	if strings.Contains(body, `data-modal-open="job-schedule"`) || strings.Contains(body, "Run again") {
		t.Error("readonly's job detail must not show operator actions")
	}
}

// TestJobDetailHidesScheduleForSourceJob: a job that ran against a --source
// working directory cannot be replayed as a shell schedule (no Dir field on
// ScheduleArgs), so the Schedule… trigger must not render for it, though Run
// again still does — now a resubmit modal that names the reverse-mapped source.
func TestJobDetailHidesScheduleForSourceJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	if _, err := h.store.CreateSource(ctx, store.Source{
		Name: "tree", Kind: store.SourcePath, URI: "/srv/tree", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "command", Kind: executor.KindAnsible, Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}, Dir: "/srv/tree"},
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	// /srv/tree does not exist, so the shell driver fails the run — the
	// terminal state under test here is "failed", not "succeeded"; either is
	// terminal, and terminal is all jobDetail's action row gates on.
	waitJobTerminal(t, h, job.ID, 3*time.Second)

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if strings.Contains(body, `data-modal-open="job-schedule"`) {
		t.Error("a source-context job must not offer Schedule…")
	}
	if !strings.Contains(body, `?rerun=1`) || !strings.Contains(body, `id="job-rerun"`) {
		t.Error("Rerun must open the resubmit modal (the ?rerun=1 JS-off path)")
	}
	if !strings.Contains(body, `<div><span>Source</span><b class="mono">tree</b></div>`) {
		t.Error("rerun modal must name the reverse-mapped source")
	}
	if !strings.Contains(body, `>Source</span> <b><a href="/sources/tree">tree ↗</a></b>`) {
		t.Error("job detail run card must show the (here, reverse-derived) source tile")
	}
}

// TestJobDetailShowsPersistedSource proves a job whose row carries Args.Source
// shows that name directly — no reverse Dir lookup needed — in both the jobs
// list's Source column and the job detail meta line.
func TestJobDetailShowsPersistedSource(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}, Source: "my-fleet"},
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	_, list := getBody(t, client, ts.URL+"/jobs")
	if !strings.Contains(list, `<small class="mono">my-fleet</small>`) {
		t.Errorf("jobs list did not show the persisted source:\n%s", list)
	}

	_, detail := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if !strings.Contains(detail, `>Source</span> <b>my-fleet ↗</b>`) {
		t.Errorf("job detail did not show the persisted source:\n%s", detail)
	}
}

// TestJobDetailScheduleDisabledForFailedJob: a failed job is still the
// schedulable shape (ansible, no source dir, has argv), so CanSchedule shows
// the button, but replaying a command already known to fail must not be one
// click away — the button renders disabled.
func TestJobDetailScheduleDisabledForFailedJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedSchedulableJob(t, h, []string{"/bin/false"})
	h.waitJobState(t, job.ID, store.StateFailed, 3*time.Second)

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if !strings.Contains(body, `data-modal-open="job-schedule" disabled`) {
		t.Error("Schedule must render disabled for a failed job")
	}
}

func TestJobScheduleRefusesNonAnsibleJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "prod-plan", Kind: executor.KindTofu, Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/jobs/"+itoa64(job.ID)+"/schedule",
		url.Values{"name": {"nightly"}, "cron": {"@every 5m"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tofu job schedule = %d, want 400", resp.StatusCode)
	}
}

// TestJobScheduleInvalidName400 is the F5 regression: the job-detail "Schedule
// this job" form used to 500 on an empty or invalid name, because the store's
// validation sentinel surfaced as a generic error. It now refuses a bad name
// with 400 before CreateSchedule, and no schedule row is written.
func TestJobScheduleInvalidName400(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	job := seedSchedulableJob(t, h, []string{"/bin/true"})
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	for _, name := range []string{"", "spaced name", "bad/name"} {
		resp := postForm(t, client, ts.URL+"/jobs/"+itoa64(job.ID)+"/schedule",
			url.Values{"name": {name}, "cron": {"@every 5m"}}, nil)
		body := readBody(t, resp)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("schedule with name %q = %d, want 400 (%s)", name, resp.StatusCode, body)
		}
	}

	rows, err := h.store.ListSchedules(ctx, false)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("invalid-name schedule attempts wrote %d rows, want 0", len(rows))
	}
}
