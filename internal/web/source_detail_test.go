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

// seedSourceCheckout registers a SYNCED path source whose checkout is a real
// temp directory holding one playbook, so discovery and a source-scoped run
// have a tree to resolve against and the playbook select stays enabled (a
// never-synced source would disable it — the F14 state is covered separately by
// seedGitSource). It returns the source name.
func seedSourceCheckout(t *testing.T, h *harness) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "site.yml"), []byte("- hosts: all\n  tasks: []\n"), 0o644); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	if _, err := h.store.CreateSource(context.Background(), store.Source{
		Name: "tree", Kind: store.SourcePath, URI: root, Enabled: true,
		LastSyncSHA: "0123456789abcdef",
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	return "tree"
}

// seedGitSource registers a git source with no checkout on disk and no sync
// recorded — the never-synced cold state the F5/F14 surfaces must read as a
// wait state with a verb, not a quiet dash.
func seedGitSource(t *testing.T, h *harness) string {
	t.Helper()
	if _, err := h.store.CreateSource(context.Background(), store.Source{
		Name: "git-src", Kind: store.SourceGit,
		URI: "git@host:git-src.git", Ref: "refs/heads/main", Enabled: true,
	}); err != nil {
		t.Fatalf("seed git source: %v", err)
	}
	return "git-src"
}

// TestSourceNeverSyncedSurfaces is the GATE for F5: a never-synced source reads
// as a wait state everywhere — a NEVER chip and attention row in the list, and
// "never — sync to discover playbooks" in wait on its detail page — never a
// quiet dash.
func TestSourceNeverSyncedSurfaces(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedGitSource(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, list := getBody(t, client, ts.URL+"/sources")
	if code != http.StatusOK {
		t.Fatalf("/sources = %d", code)
	}
	for _, want := range []string{
		`<span class="chip wait"><span class="dot"></span>NEVER</span>`,
		`class="attention"`,
	} {
		if !strings.Contains(list, want) {
			t.Errorf("/sources missing %q (F5)", want)
		}
	}

	code, detail := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	for _, want := range []string{
		`class="mono wait-text">never — sync to discover playbooks</dd>`,
		`exocomp source sync git-src`,
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("source detail missing %q (F5)", want)
		}
	}
}

// TestSourceLaunchDrawer is the GATE for F4/F14: the launcher is a right-anchored
// drawer opened by intent, not a wall-of-form; ?launch=1 renders it open (JS-off);
// and a never-synced source with no discovered playbooks gets a disabled select
// with the path promoted, not a dead placeholder listbox.
func TestSourceLaunchDrawer(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedGitSource(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	for _, want := range []string{
		`class="modal drawer" id="launch"`,
		`>Launch a run</button>`,
		`data-modal-open="launch"`,
		`<select id="rp-select" name="playbook" disabled>`,
		`<option value="">path — none discovered yet</option>`,
		`Unsynced source: give a path relative to the checkout.`,
		`Queue run — a path @ refs/heads/main`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("launch drawer missing %q", want)
		}
	}
	// The drawer is closed by default — no open attribute without ?launch=1.
	if strings.Contains(page, `<dialog class="modal drawer" id="launch" aria-labelledby="launch-title" open`) {
		t.Error("launch drawer rendered open without ?launch=1 (JS-off must opt in)")
	}

	code, openPage := getBody(t, client, ts.URL+"/sources/"+name+"?launch=1")
	if code != http.StatusOK {
		t.Fatalf("/sources/%s?launch=1 = %d", name, code)
	}
	if !strings.Contains(openPage, `<dialog class="modal drawer" id="launch" aria-labelledby="launch-title" open`) {
		t.Error("?launch=1 did not render the drawer open (JS-off baseline)")
	}

	// A readonly session never sees the drawer at all.
	ro := h.mint(t, "ro", "readonly")
	roClient := login(t, ts.URL, ro)
	_, roPage := getBody(t, roClient, ts.URL+"/sources/"+name)
	if strings.Contains(roPage, `id="launch"`) {
		t.Error("readonly session sees the launch drawer")
	}
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
	// The Command tab posts the timeout the route parses — the field the
	// drawer rebuild dropped while the Playbook tab kept it (MED).
	if !strings.Contains(body, `<label for="rc-timeout">Timeout (optional, e.g. 10m)</label>`) ||
		!strings.Contains(body, `<input id="rc-timeout" name="timeout" type="text" autocomplete="off" placeholder="no limit">`) {
		t.Error("launch drawer Command tab lost its Timeout field (MED)")
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

// TestSourceDetailRendersBreadcrumb: the source detail page carries a
// Sources / <name> breadcrumb above the header and no longer the old "All
// sources" button beside the <h1>.
func TestSourceDetailRendersBreadcrumb(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, body := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("source detail = %d, want 200", code)
	}
	if !strings.Contains(body, `<nav class="breadcrumb" aria-label="Breadcrumb"> <a href="/sources">Sources</a><span class="crumb-sep" aria-hidden="true">/</span> <span aria-current="page">tree</span></nav>`) {
		t.Errorf("source detail missing Sources / tree breadcrumb, got:\n%s", body)
	}
	if strings.Contains(body, `class="btn" href="/sources"`) {
		t.Error("source detail must not render the old All sources button")
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
	if jobs[0].Args.Source != name {
		t.Errorf("Args.Source = %q, want %q", jobs[0].Args.Source, name)
	}
}

// TestSourceRunPrivilegedRefusedForReadonly: a readonly token asking for a
// privileged (unsandboxed) run is refused by the same operator-scope gate every
// run on this surface passes, and enqueues nothing.
func TestSourceRunPrivilegedRefusedForReadonly(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"site.yml"}, "privileged": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly privileged run = %d, want 403", resp.StatusCode)
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused privileged run still enqueued %d job(s)", len(jobs))
	}
}

// TestSourceRunPlaybookPrivilegedMarksJob: an operator's privileged run sets the
// marker on the row; a run without the box left unchecked stays sandboxed
// (secure default).
func TestSourceRunPlaybookPrivilegedMarksJob(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"site.yml"}, "privileged": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("operator privileged run = %d, want 303", resp.StatusCode)
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || !jobs[0].Args.Privileged {
		t.Fatalf("operator privileged run did not mark the row: %+v", jobs)
	}

	// The box unchecked stays sandboxed: a second run with no privileged field
	// lands a distinct, non-privileged job (secure default).
	resp = postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"site.yml"}}, nil)
	_ = resp.Body.Close()
	plain, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	for _, j := range plain {
		if j.ID != jobs[0].ID && j.Args.Privileged {
			t.Errorf("a run with the box unchecked defaulted to privileged: %+v", j)
		}
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
	if jobs[0].Args.Source != name {
		t.Errorf("Args.Source = %q, want %q", jobs[0].Args.Source, name)
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

// seedUnsyncedPath registers a PATH source with a checkout on disk but no sync
// recorded: discovery finds real playbooks, yet the source has never synced —
// the F14 state where the select must be disabled anyway (never-synced = no
// trusted tree).
func seedUnsyncedPath(t *testing.T, h *harness) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "site.yml"), []byte("- hosts: all\n  tasks: []\n"), 0o644); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	if _, err := h.store.CreateSource(context.Background(), store.Source{
		Name: "unsynced", Kind: store.SourcePath, URI: root, Enabled: true,
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	return "unsynced"
}

// TestSourcePlaybookSelectDisabledUnsynced is the GATE for F14: a never-synced
// source gets the disabled playbook select + the path-promoted meta line even
// when discovery found playbooks in the checkout — the select is a dead control
// until the source has synced.
func TestSourcePlaybookSelectDisabledUnsynced(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedUnsyncedPath(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	for _, want := range []string{
		`<select id="rp-select" name="playbook" disabled>`,
		`Unsynced source: give a path relative to the checkout.`,
		`site.yml`, // discovery still lists the checkout's playbook in the card
	} {
		if !strings.Contains(page, want) {
			t.Errorf("unsynced source missing F14 marker %q", want)
		}
	}
}
