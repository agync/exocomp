package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// seedSourceCheckout registers a path source whose checkout is a real temp
// directory holding one playbook, so discovery and a source-scoped run have a
// tree to resolve against. It returns the source name.
func seedSourceCheckout(t *testing.T, h *harness) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "site.yml"), []byte("- hosts: all\n  tasks: []\n"), 0o644); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	if _, err := h.store.CreateSource(context.Background(), store.Source{
		Name: "tree", Kind: store.SourcePath, URI: root, Enabled: true,
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	return "tree"
}

// TestSourceDetailRendersLauncherForOperator: an operator's detail page shows
// both the source-scoped run forms; a readonly token sees the header and a
// read-only note, no launcher.
func TestSourceDetailRendersLauncherForOperator(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)

	op := h.mint(t, "laptop", "operator")
	opClient := login(t, ts.URL, op)
	code, body := getBody(t, opClient, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("operator detail = %d, want 200", code)
	}
	if !strings.Contains(body, `action="/sources/tree/run/playbook"`) ||
		!strings.Contains(body, `action="/sources/tree/run/command"`) {
		t.Error("operator's source detail must render both run forms")
	}
	// Discovery lists the seeded playbook as a <select> option.
	if !strings.Contains(body, `>site.yml</option>`) {
		t.Errorf("playbook select must list the discovered playbook, got:\n%s", body)
	}

	ro := h.mint(t, "ro", "readonly")
	roClient := login(t, ts.URL, ro)
	code, body = getBody(t, roClient, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("readonly detail = %d, want 200", code)
	}
	if strings.Contains(body, `action="/sources/tree/run/playbook"`) {
		t.Error("readonly's source detail must not render the run forms")
	}
	if !strings.Contains(body, "tree") {
		t.Error("readonly still sees the source header")
	}
}

// TestSourceDetailUnknownSource: a detail page for a source that does not exist
// is a 404, not a render.
func TestSourceDetailUnknownSource(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, _ := getBody(t, client, ts.URL+"/sources/nope")
	if code != http.StatusNotFound {
		t.Fatalf("unknown source detail = %d, want 404", code)
	}
}

// TestSourceRunReadonlyForbidden: a readonly token may not enqueue a run from a
// source.
func TestSourceRunReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"site.yml"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly run = %d, want 403", resp.StatusCode)
	}
}

// TestSourceRunCrossOriginForbidden: a cross-site form post is refused before
// the form is even parsed.
func TestSourceRunCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"site.yml"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin run = %d, want 403", resp.StatusCode)
	}
}

// TestSourceRunPlaybookEnqueues: an operator's browser form post enqueues a
// playbook job resolved against the source checkout, and lands a 303 to the new
// job's page.
func TestSourceRunPlaybookEnqueues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"site.yml"}, "limit": {"web"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("playbook run = %d, want 303 (%s)", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/jobs/") {
		t.Fatalf("redirect = %q, want /jobs/<id>", loc)
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want one playbook job enqueued, got %d", len(jobs))
	}
	if !strings.Contains(strings.Join(jobs[0].Args.Argv, " "), "site.yml") {
		t.Errorf("argv = %v, want the resolved playbook path", jobs[0].Args.Argv)
	}
}

// TestSourceRunPlaybookFreePath: the free-path input covers a playbook discovery
// missed; it is resolved against the checkout the same way.
func TestSourceRunPlaybookFreePath(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	// The <select> yields the "other path…" sentinel; the free-path input wins.
	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"__other__"}, "playbook_path": {"site.yml"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("free-path run = %d, want 303", resp.StatusCode)
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want one playbook job enqueued, got %d", len(jobs))
	}
}

// TestSourceRunPlaybookRequiresPlaybook: an empty playbook (neither select nor
// free-path) is a 400, and no job is created.
func TestSourceRunPlaybookRequiresPlaybook(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty playbook run = %d, want 400", resp.StatusCode)
	}
}

// TestSourceRunCommandEnqueues: an operator's command form post enqueues an
// ad-hoc ansible job through service.RunAdhoc, with the source checkout as its
// working directory.
func TestSourceRunCommandEnqueues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/command",
		url.Values{"host": {"web"}, "command": {"uptime"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("command run = %d, want 303 (%s)", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/jobs/") {
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

// TestSourceRunCommandRequiresCommand: an empty command field is a 400.
func TestSourceRunCommandRequiresCommand(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/command", url.Values{"host": {"web"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty command run = %d, want 400", resp.StatusCode)
	}
}

// TestSourceRunCommandRequireApprovalGates: checking "require approval" routes
// the job through the gate and redirects to /approvals instead of /jobs/<id>.
func TestSourceRunCommandRequireApprovalGates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/command",
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

// TestRunSurfaceRemoved: the standalone /run surface is gone — GET /run 404s and
// the old POST /run/* browser routes no longer exist. All runs are now launched
// in source context.
func TestRunSurfaceRemoved(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	if code, _ := getBody(t, client, ts.URL+"/run"); code != http.StatusNotFound {
		t.Fatalf("GET /run = %d, want 404 (removed)", code)
	}
	resp := postForm(t, client, ts.URL+"/run/playbook", url.Values{"playbook": {"site.yml"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /run/playbook = %d, want 404 (removed)", resp.StatusCode)
	}
}
