package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// seedTerminalJob writes a job whose argv[0] is /bin/true (so the harness queue
// runs it to succeeded) and waits for it to finish, the finished shape "Run
// again" reconstructs from. args carries whatever run params the rerun must
// clone (Source, Inventory, Privileged, …).
func seedTerminalJob(t *testing.T, h *harness, args store.JobArgs) store.Job {
	t.Helper()
	job, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual", Args: args,
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return waitJobTerminal(t, h, job.ID, 3*time.Second)
}

// rerunJob returns the single job whose trigger marks it a rerun, failing if
// there is not exactly one.
func rerunJob(t *testing.T, h *harness) store.Job {
	t.Helper()
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	var found []store.Job
	for _, j := range jobs {
		if j.Trigger == rerunTrigger {
			found = append(found, j)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one rerun job, got %d", len(found))
	}
	return found[0]
}

// TestJobRerunClonesEquivalentJob: a resubmit with the modal's own prefilled
// values (the drift flags left on, limit left as it was) reproduces the original
// run — same argv, same carried names — under a fresh job with the rerun trigger.
func TestJobRerunClonesEquivalentJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	orig := seedTerminalJob(t, h, store.JobArgs{
		Argv:             []string{"/bin/true", "--limit", "web", "--check"},
		Source:           "tree",
		Inventory:        "prod-inv",
		VaultProject:     "vault-a",
		RunSecretBinding: "creds-b",
		RunnerName:       "small",
		Timeout:          store.Duration(30 * time.Second),
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	resp := postForm(t, client, ts.URL+"/jobs/"+itoa64(orig.ID)+"/rerun",
		url.Values{"check": {"1"}, "limit": {"web"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rerun = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/jobs/") {
		t.Fatalf("redirect = %q, want /jobs/<id>", loc)
	}

	got := rerunJob(t, h)
	if got.ID == orig.ID {
		t.Fatal("rerun must be a new job, not the original")
	}
	if want := []string{"/bin/true", "--limit", "web", "--check"}; !reflect.DeepEqual(got.Args.Argv, want) {
		t.Errorf("rerun argv = %v, want %v", got.Args.Argv, want)
	}
	if got.Args.Source != "tree" || got.Args.Inventory != "prod-inv" ||
		got.Args.VaultProject != "vault-a" || got.Args.RunSecretBinding != "creds-b" ||
		got.Args.RunnerName != "small" || time.Duration(got.Args.Timeout) != 30*time.Second {
		t.Errorf("rerun did not carry the original run's params unchanged: %+v", got.Args)
	}
	if got.Args.Privileged {
		t.Error("a rerun of a sandboxed job must stay sandboxed")
	}
}

// TestJobRerunToggleOverridesAdd: turning the check/diff/privileged toggles on
// adds --check/--diff to the cloned argv and sets the privileged marker.
func TestJobRerunToggleOverridesAdd(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	orig := seedTerminalJob(t, h, store.JobArgs{Argv: []string{"/bin/true"}})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	resp := postForm(t, client, ts.URL+"/jobs/"+itoa64(orig.ID)+"/rerun",
		url.Values{"check": {"1"}, "diff": {"1"}, "privileged": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rerun = %d, want 303", resp.StatusCode)
	}

	got := rerunJob(t, h)
	if !hasFlag(got.Args.Argv, "--check") || !hasFlag(got.Args.Argv, "--diff") {
		t.Errorf("toggles on must add --check and --diff, got argv %v", got.Args.Argv)
	}
	if !got.Args.Privileged {
		t.Error("privileged toggle on must set the row's privileged marker")
	}
}

// TestJobRerunToggleOverridesRemove: leaving the toggles off strips --check and
// --diff from a run that had them and de-escalates a privileged run — the client
// is never trusted to keep an escalation.
func TestJobRerunToggleOverridesRemove(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	orig := seedTerminalJob(t, h, store.JobArgs{
		Argv: []string{"/bin/true", "--check", "--diff"}, Privileged: true,
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	resp := postForm(t, client, ts.URL+"/jobs/"+itoa64(orig.ID)+"/rerun",
		url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rerun = %d, want 303", resp.StatusCode)
	}

	got := rerunJob(t, h)
	if hasFlag(got.Args.Argv, "--check") || hasFlag(got.Args.Argv, "--diff") {
		t.Errorf("toggles off must strip --check/--diff, got argv %v", got.Args.Argv)
	}
	if got.Args.Privileged {
		t.Error("privileged toggle off must de-escalate the rerun")
	}
}

// TestJobRerunReadonlyForbidden: a readonly token's rerun is refused before the
// privileged flag is honored — privileged or not — and enqueues nothing. This is
// the escalation gate: the client hiding the toggle is never trusted.
func TestJobRerunReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	orig := seedTerminalJob(t, h, store.JobArgs{Argv: []string{"/bin/true"}})

	ro := login(t, ts.URL, h.mint(t, "ro", "readonly"))
	for _, form := range []url.Values{{}, {"privileged": {"1"}}} {
		resp := postForm(t, ro, ts.URL+"/jobs/"+itoa64(orig.ID)+"/rerun", form, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("readonly rerun %v = %d, want 403", form, resp.StatusCode)
		}
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != orig.ID {
		t.Fatalf("a refused rerun still enqueued a job: %+v", jobs)
	}
}

// TestJobRerunRejectsUnrunnable: an unknown job is a 404, a non-terminal
// (awaiting-approval) job is a 409, and a terminal but argv-less job is a 400 —
// nothing to clone.
func TestJobRerunRejectsUnrunnable(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/jobs/999999/rerun", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown job rerun = %d, want 404", resp.StatusCode)
	}

	// A gated job sits in awaiting_approval — non-terminal, so it cannot be rerun.
	gated, _ := h.gatedJob(t, time.Minute)
	resp = postForm(t, client, ts.URL+"/jobs/"+itoa64(gated.ID)+"/rerun", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("non-terminal rerun = %d, want 409", resp.StatusCode)
	}

	// A terminal job that carries no argv has nothing to clone. The queue settles
	// an empty-argv run to a terminal state on its own.
	argvless, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
	})
	if err != nil {
		t.Fatalf("seed argv-less job: %v", err)
	}
	waitJobTerminal(t, h, argvless.ID, 3*time.Second)
	resp = postForm(t, client, ts.URL+"/jobs/"+itoa64(argvless.ID)+"/rerun", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("argv-less rerun = %d, want 400", resp.StatusCode)
	}
}

// TestJobRerunModalPrefills: the resubmit modal renders on a succeeded job's
// detail with each toggle prefilled to the original run's flags (Check from
// --check in argv, Privileged from the row) and limit/tags prefilled from the
// argv. It is also the shared toggle's JS-off proof: every switch is a real
// <input type="checkbox"> form field.
func TestJobRerunModalPrefills(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	job := seedTerminalJob(t, h, store.JobArgs{
		Argv: []string{"/bin/true", "--limit", "web", "--tags", "deploy", "--check"}, Privileged: true,
	})
	h.waitJobState(t, job.ID, store.StateSucceeded, 3*time.Second)

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))

	if !strings.Contains(body, `action="/jobs/`+itoa64(job.ID)+`/rerun"`) {
		t.Fatal("succeeded job detail must render the rerun form")
	}
	// The switch is a real checkbox form field (JS-off fallback), prefilled on
	// for the flags the original run carried.
	if !strings.Contains(body, `<input type="checkbox" role="switch" name="check" value="1" class="toggle-cb" checked>`) {
		t.Error("check toggle must be a checkbox prefilled on for a --check run")
	}
	if !strings.Contains(body, `<input type="checkbox" role="switch" name="privileged" value="1" class="toggle-cb" checked>`) {
		t.Error("privileged toggle must be a checkbox prefilled on for a privileged run")
	}
	if strings.Contains(body, `name="diff" value="1" class="toggle-cb" checked`) {
		t.Error("diff toggle must be off for a run without --diff")
	}
	if !strings.Contains(body, `name="limit" type="text" autocomplete="off" placeholder="host or group pattern" value="web"`) {
		t.Error("limit field must be prefilled from the argv")
	}
	if !strings.Contains(body, `name="tags" type="text" autocomplete="off" value="deploy"`) {
		t.Error("tags field must be prefilled from the argv")
	}
}

// TestSourceLauncherTogglesAreCheckboxes: the source launcher's run-options are
// the same shared switch — real checkbox form fields, so a JS-off browser still
// submits them.
func TestSourceLauncherTogglesAreCheckboxes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	_, body := getBody(t, client, ts.URL+"/sources/"+name)
	for _, field := range []string{"check", "diff", "privileged"} {
		if !strings.Contains(body, `<input type="checkbox" role="switch" name="`+field+`" value="1" class="toggle-cb"`) {
			t.Errorf("launcher %q option must render as a real checkbox switch", field)
		}
	}
}
