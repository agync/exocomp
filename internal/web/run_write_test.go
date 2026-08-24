package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestRunReadonlyForbidden: a readonly token may not enqueue a playbook run.
func TestRunReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/run/playbook",
		url.Values{"playbook": {"/srv/site.yml"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly run = %d, want 403", resp.StatusCode)
	}
}

// TestRunCrossOriginForbidden: a cross-site form post is refused before the
// form is even parsed.
func TestRunCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/run/playbook",
		url.Values{"playbook": {"/srv/site.yml"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin run = %d, want 403", resp.StatusCode)
	}
}

// TestRunPlaybookEnqueuesThroughService: an operator's browser form post
// enqueues a playbook job through the same service.RunPlaybook the CLI and
// the /api/v1 write API use, and lands a 303 to the new job's page.
func TestRunPlaybookEnqueuesThroughService(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/run/playbook",
		url.Values{"playbook": {"/srv/site.yml"}, "limit": {"web"}, "confirm_token": {op}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("playbook run = %d, want 303 (%s)", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/jobs/") {
		t.Fatalf("redirect = %q, want /jobs/<id>", loc)
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want one playbook job enqueued, got %d", len(jobs))
	}
	if !strings.Contains(strings.Join(jobs[0].Args.Argv, " "), "/srv/site.yml") {
		t.Errorf("argv = %v, want the playbook path", jobs[0].Args.Argv)
	}
}

// TestRunPlaybookRequiresPlaybook: an empty playbook field is a 400, and no
// job is created.
func TestRunPlaybookRequiresPlaybook(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/run/playbook", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty playbook run = %d, want 400", resp.StatusCode)
	}
}

// TestRunCommandEnqueuesThroughService: an operator's command form post
// enqueues an ad-hoc ansible job through service.RunAdhoc.
func TestRunCommandEnqueuesThroughService(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/run/command",
		url.Values{"host": {"web"}, "command": {"uptime"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("command run = %d, want 303 (%s)", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/jobs/") {
		t.Fatalf("redirect = %q, want /jobs/<id>", loc)
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "command"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want one command job enqueued, got %d", len(jobs))
	}
	argv := strings.Join(jobs[0].Args.Argv, " ")
	if !strings.Contains(argv, "web") || !strings.Contains(argv, "uptime") {
		t.Errorf("argv = %v, want the host pattern and command", jobs[0].Args.Argv)
	}
}

// TestRunCommandRequiresCommand: an empty command field is a 400, and no job
// is created.
func TestRunCommandRequiresCommand(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/run/command", url.Values{"host": {"web"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty command run = %d, want 400", resp.StatusCode)
	}
}

// TestRunCommandRequireApprovalGates: checking "require approval" routes the
// job through the gate and redirects to /approvals instead of /jobs/<id>.
func TestRunCommandRequireApprovalGates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/run/command",
		url.Values{"host": {"web"}, "command": {"reboot"}, "require_approval": {"1"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("gated command run = %d, want 303 (%s)", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/approvals") {
		t.Fatalf("redirect = %q, want /approvals", loc)
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "command"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != store.StateAwaitingApproval {
		t.Fatalf("want one job awaiting_approval, got %+v", jobs)
	}
}

// TestRunFormRendersByScope: the GET /run page shows the write forms for an
// operator and a read-only note for a readonly token.
func TestRunFormRendersByScope(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	op := h.mint(t, "laptop", "operator")
	opClient := login(t, ts.URL, op)
	_, body := getBody(t, opClient, ts.URL+"/run")
	if !strings.Contains(body, `action="/run/playbook"`) || !strings.Contains(body, `action="/run/command"`) {
		t.Error("operator's /run page must render both write forms")
	}

	ro := h.mint(t, "ro", "readonly")
	roClient := login(t, ts.URL, ro)
	_, body = getBody(t, roClient, ts.URL+"/run")
	if strings.Contains(body, `action="/run/playbook"`) {
		t.Error("readonly's /run page must not render the write forms")
	}
}
