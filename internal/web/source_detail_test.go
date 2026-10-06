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
	"time"

	"exocomp.dev/exocomp/internal/queue"
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
		Name: "tree", Kind: store.SourcePath, URI: root, Ref: "main",
		Enabled: true, LastSyncSHA: "0123456789abcdef",
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

// TestSourcePlaybookLauncherPreselectsFirst: the Playbook select carries the
// real playbook list with the FIRST found playbook preselected — no placeholder
// option (SPEC §5.2) — so the launcher states one concrete run before any
// input. The meta reads "N playbooks found at {ref}".
func TestSourcePlaybookLauncherPreselectsFirst(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	for _, want := range []string{
		`<option value="site.yml" selected>site.yml</option>`,
		"1 playbooks found at main",
		`>Queue run</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("playbook launcher missing %q", want)
		}
	}
	// The button is the bare verb: the run's ask lives in the footer line, so a
	// long collection path can never wrap the primary. A reintroduced
	// "Queue run — <path>" label is the mutation this gate must catch.
	if strings.Contains(page, "Queue run —") {
		t.Error("Queue run button must not restate the playbook path")
	}
	if strings.Contains(page, "— choose a discovered playbook —") {
		t.Error("playbook select still renders a placeholder option")
	}
}

// TestSourcePlaybookLauncherMeta: the launcher's meta names the source's last
// playbook run ("last run job #N · state"), the Limit field reads the
// --limit contract, the Egress field is prefilled from the last run's ports
// (the source's current posture), the Tolerate checkbox states its degraded
// outcome, and the CLI echo repeats the primary's whole ask.
func TestSourcePlaybookLauncherMeta(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	job, err := h.queue.Enqueue(context.Background(), queue.Spec{
		Runbook: "playbook", Kind: "shell", Trigger: "manual",
		Argv:   []string{"/bin/sh", "-c", "echo ran; exit 0"},
		Source: name, EgressPorts: []int{22, 443},
	})
	if err != nil {
		t.Fatalf("enqueue playbook job: %v", err)
	}
	h.waitJobState(t, job.ID, store.StateSucceeded, 5*time.Second)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	for _, want := range []string{
		"1 playbooks found at main · last run job #" + itoa64(job.ID) + " · succeeded",
		`placeholder="all hosts"`,
		"ansible --limit pattern",
		`value="22, 443"`,
		"Ports the runner may reach — same as --egress.",
		`may end <span class="wait-text">DEGRADED</span>`,
		"asks nothing else · same as exocomp run tree site.yml --egress 22,443",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("playbook launcher missing %q", want)
		}
	}
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
		`<select id="rp-select" name="playbook" disabled aria-describedby="rp-select-meta">`,
		`<option value="">path — none discovered yet</option>`,
		`this source has never synced. Sync brings the checkout down and reads its layout.`,
		`asks nothing else · same as exocomp run git-src a path · Esc closes`,
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
	// drawer rebuild dropped while the Playbook tab kept it (MED). It now
	// renders through fieldInput with its always-present meta line.
	if !strings.Contains(body, `<input class="control" id="rc-timeout" name="timeout" value="" spellcheck="false" placeholder="no limit" aria-describedby="rc-timeout-meta">`) ||
		!strings.Contains(body, `id="rc-timeout-meta"`) {
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

// TestSourceLaunchDrawerTolerateToggle: the launch drawer's Options fold
// carries the "Tolerate unreachable hosts" switch, and checking it enqueues a
// playbook job with TolerateUnreachable set — the web half of the CLI's
// --tolerate-unreachable parity (1c).
func TestSourceLaunchDrawerTolerateToggle(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	for _, want := range []string{
		`name="tolerate_unreachable" value="1" class="toggle-cb"`,
		`may end <span class="wait-text">DEGRADED</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("launch drawer missing %q", want)
		}
	}

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"site.yml"}, "tolerate_unreachable": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("tolerating run = %d, want 303", resp.StatusCode)
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || !jobs[0].Args.TolerateUnreachable {
		t.Fatalf("tolerating run did not mark the row: %+v", jobs)
	}

	// The box unchecked stays non-tolerating (secure default).
	resp = postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"site.yml"}}, nil)
	_ = resp.Body.Close()
	plain, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	for _, j := range plain {
		if j.ID != jobs[0].ID && j.Args.TolerateUnreachable {
			t.Errorf("a run with the box unchecked defaulted to tolerating: %+v", j)
		}
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
// free-path) re-renders the drawer at 422 with the select's meta line refusing
// — the forms pattern — and no job is created.
func TestSourceRunPlaybookRequiresPlaybook(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook", url.Values{}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty playbook run = %d, want 422 (%s)", resp.StatusCode, body)
	}
	for _, want := range []string{
		`id="rp-select-meta" class="meta is-error"`,
		"Choose a playbook, or give a path relative to the checkout.",
		`open`, // the drawer re-renders open
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty-playbook 422 missing %q", want)
		}
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("an empty-playbook run created %d job(s)", len(jobs))
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

// TestSourceRunCommandRequiresCommand: an empty command field re-renders the
// drawer at 422 with the field's is-error meta line — the forms pattern.
func TestSourceRunCommandRequiresCommand(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/command", url.Values{"host": {"web"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty command run = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `id="rc-command-meta" class="meta is-error"`) ||
		!strings.Contains(body, "Give the command to run.") {
		t.Errorf("empty-command 422 missing the field refusal:\n%s", body)
	}
	if !strings.Contains(body, `value="web"`) {
		t.Errorf("empty-command 422 lost the typed host value:\n%s", body)
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

// TestSourceRunPlaybook422KeepsValues pins the forms pattern on the launcher's
// Playbook tab: a junk egress re-renders the drawer at 422 with the typed
// limit/timeout values and the field refusal kept, and a posted confirm_token
// never echoes.
func TestSourceRunPlaybook422KeepsValues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"site.yml"}, "limit": {"web"}, "egress": {"99999"},
			"timeout": {"10m"}, "confirm_token": {"exo_9_never_echo"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("junk-egress playbook run = %d, want 422 (%s)", resp.StatusCode, body)
	}
	for _, want := range []string{
		`value="web"`, // the typed limit survives
		`value="10m"`, // the typed timeout survives
		`id="rp-egress-meta" class="meta is-error"`,
		"Egress ports are numbers from 1 to 65535 — like 22, 443.",
		`open`, // the drawer re-renders open
	} {
		if !strings.Contains(body, want) {
			t.Errorf("playbook 422 missing %q", want)
		}
	}
	if strings.Contains(body, "never_echo") {
		t.Error("422 re-render echoed a posted confirm_token")
	}
}

// TestSourceRunCommand422KeepsValues pins the forms pattern on the launcher's
// Command tab: a bad timeout re-renders the drawer at 422 with the typed
// host/command values kept and the timeout field's refusal on its meta line.
func TestSourceRunCommand422KeepsValues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/command",
		url.Values{"host": {"web"}, "command": {"uptime"}, "timeout": {"5x"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad-timeout command run = %d, want 422 (%s)", resp.StatusCode, body)
	}
	for _, want := range []string{
		`value="web"`,    // the typed host survives
		`value="uptime"`, // the typed command survives
		`value="5x"`,     // the typed timeout survives
		`id="rc-timeout-meta" class="meta is-error"`,
		"The timeout is a duration — like 10m or 1h.",
		`open`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("command 422 missing %q", want)
		}
	}

	// An empty host is refused on the host field, keeping the command.
	resp = postForm(t, client, ts.URL+"/sources/"+name+"/run/command",
		url.Values{"host": {""}, "command": {"uptime"}}, nil)
	body = readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty-host command run = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `id="rc-host-meta" class="meta is-error"`) ||
		!strings.Contains(body, "Give a host pattern") {
		t.Errorf("empty-host 422 missing the host refusal:\n%s", body)
	}
}

// seedUnsyncedPath registers a PATH source with a checkout on disk but no sync
// recorded: discovery finds real playbooks, and because a path source has no
// sync verb at all (`source sync` refuses a non-git source), the launcher's
// playbook select is enabled — the tree is already present and read directly.
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

// TestSourcePathSelectEnabledWhenPlaybooksFound is the gate for the path-source
// launch fix: a path source is never "never-synced" (there is no sync verb for
// it), so when discovery finds playbooks the launcher select is ENABLED and
// lists them — it must not be a dead "none discovered yet" control. The card
// and the launcher must agree.
func TestSourcePathSelectEnabledWhenPlaybooksFound(t *testing.T) {
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
		`<select id="rp-select" name="playbook" aria-describedby="rp-select-meta">`, // enabled, not disabled
		`>site.yml</option>`, // the discovered playbook is selectable
	} {
		if !strings.Contains(page, want) {
			t.Errorf("path source launcher missing %q", want)
		}
	}
	for _, banned := range []string{
		`<select id="rp-select" name="playbook" disabled>`,
		`path — none discovered yet`,
		`Unsynced source: give a path relative to the checkout.`,
	} {
		if strings.Contains(page, banned) {
			t.Errorf("path source launcher must not contain %q", banned)
		}
	}
	// A path source has no sync to report: no "never — sync to discover
	// playbooks" wait fact, and the list shows no NEVER chip.
	if strings.Contains(page, "never — sync to discover playbooks") {
		t.Error("path source detail must not claim it needs a sync")
	}
	if !strings.Contains(page, "n/a (path source)") {
		t.Error("path source detail must show n/a (path source) for Last sync")
	}
	code, list := getBody(t, client, ts.URL+"/sources")
	if code != http.StatusOK {
		t.Fatalf("/sources = %d", code)
	}
	if strings.Contains(list, "NEVER") {
		t.Errorf("path source must not wear a NEVER chip in the list, got:\n%s", list)
	}
}

// TestSourceLaunchDrawerPhoneFixes is the GATE for P12/P13: the launch
// drawer's phone pass — no leaked "·" before the DEGRADED outcome, a chevron
// on the More disclosure, the drawer ending above the powerline foot with its
// own sticky footer, and the playbook rows 44px tappable with a chevron. The
// MUTATIONS — restoring the "·", dropping the chevron, or reverting the
// playbook rows to plain text — must red this test.
func TestSourceLaunchDrawerPhoneFixes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	if strings.Contains(page, `<p class="meta">· may end`) {
		t.Error("the tolerate meta must not open with a leaked '·'")
	}
	if !strings.Contains(page, `<p class="meta">may end <span class="wait-text">DEGRADED</span></p>`) {
		t.Error("the tolerate meta must read 'may end DEGRADED'")
	}
	if !strings.Contains(page, `<summary>More <svg class="chev"`) {
		t.Error("the More disclosure summary must carry a chevron glyph")
	}
	if !strings.Contains(page, `<p class="opts-help">path, inventory, tags, secrets, run modes</p>`) {
		t.Error("the More contents must move to a 12px helper under the summary")
	}
	// P13: the discovered playbook renders as an operator tappable row that
	// opens the launcher prefilled (launch=1 opens the drawer, playbook=
	// preselects), with the › chevron.
	if !strings.Contains(page, `<a class="playbook-row" href="/sources/`+name+`?launch=1&amp;tab=playbook&amp;playbook=site.yml">site.yml<span class="chev" aria-hidden="true">›</span></a>`) {
		t.Error("the discovered playbook must render as a tappable row link with the chevron")
	}

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		".modal.drawer { inset: 0 0 36px auto; height: auto; max-height: none; }",
		".modal.drawer .drawer-actions { flex: none;",
		"env(safe-area-inset-bottom)",
		".modal.drawer .writeform input { height: var(--row-h); min-height: var(--row-h); }",
		".playbooklist .playbook-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; min-height: 44px;",
		".playbooklist .playbook-row:hover { background: var(--surface-tray);",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the drawer phone fix %q", want)
		}
	}
}

// TestSourceLaunchDrawerFooterOutsideBody pins the drawer primary's phone home:
// the Queue run footer is a DIALOG SIBLING below the scroll body, not a sticky
// child of the form — so the body's scroll area ends where the footer begins and
// the last field group (the privileged block) can never hide under it. The
// submit button lives outside its form, associated by id through form="…".
// Layout is browser-verified; this guard pins the markup and the rules. The
// mutation (a sticky footer back inside the form) reds this test.
func TestSourceLaunchDrawerFooterOutsideBody(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	for _, want := range []string{
		`<div class="drawer-actions" data-foot="playbook"`,
		`<div class="drawer-actions" data-foot="command"`,
		`<form id="launch-pb-form"`,
		`<form id="launch-cmd-form"`,
		`form="launch-pb-form"`,
		`form="launch-cmd-form"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("drawer missing %q", want)
		}
	}

	_, app := getBody(t, client, ts.URL+"/assets/app.css")
	for _, want := range []string{
		".modal.drawer .modal-body { flex: 1 1 auto;",
		".modal.drawer .drawer-actions { flex: none;",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.css missing the drawer flex rule %q", want)
		}
	}
	if strings.Contains(app, ".modal.drawer .drawer-actions { position: sticky;") {
		t.Error("the drawer footer must not be sticky; it is a flex sibling of the scroll body")
	}
}

// TestSourceDeleteConfirmNamesCacheRemoval: the source detail's Delete
// confirmation states that deleting a source also removes its cached Python
// venv and ansible collections (ADR 0015), so an operator sees the full scope
// of the destructive action before confirming.
func TestSourceDeleteConfirmNamesCacheRemoval(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	if !strings.Contains(page, `data-confirm="Delete source tree? Its cached Python venv and ansible collections are removed with it."`) {
		t.Errorf("source delete confirm must name the cache removal, got:\n%s", page)
	}
}
