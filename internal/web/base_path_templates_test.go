package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestBasePathTemplateAssetsAndLogin pins the login surface's template-emitted
// sites under a base path: the embedded stylesheets and the login form action
// render under the prefix, and nothing double-prefixes.
func TestBasePathTemplateAssetsAndLogin(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	code, body := getBody(t, &http.Client{}, ts.URL+"/exocomp/login")
	if code != http.StatusOK {
		t.Fatalf("GET /exocomp/login = %d, want 200", code)
	}
	for _, want := range []string{
		`href="/exocomp/assets/tokens.css?v=`,
		`href="/exocomp/assets/app.css?v=`,
		`action="/exocomp/login"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing prefixed %q", want)
		}
	}
	if strings.Contains(body, `action="/login"`) {
		t.Error("login form action not prefixed")
	}
	if strings.Contains(body, "/exocomp/exocomp") {
		t.Error("login page double-prefixed the base path")
	}
}

// TestBasePathTemplateNavAndForms pins the authenticated chrome's
// template-emitted sites: the nav links, brand links, powerline, the tabs,
// and a resource add-form action all render under the prefix.
func TestBasePathTemplateNavAndForms(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := baseLogin(t, ts.URL, "/exocomp", h.mint(t, "laptop", "operator"))

	code, body := getBody(t, op, ts.URL+"/exocomp/checks")
	if code != http.StatusOK {
		t.Fatalf("GET /exocomp/checks = %d, want 200", code)
	}
	for _, want := range []string{
		`href="/exocomp/"`,
		`href="/exocomp/approvals"`,
		`href="/exocomp/jobs"`,
		`href="/exocomp/events"`,
		`href="/exocomp/checks"`,
		`href="/exocomp/checks?tab=netgroups"`,
		`href="/exocomp/rules"`,
		`href="/exocomp/system"`,
		`href="/exocomp/assets/tokens.css?v=`,
		`href="/exocomp/assets/app.css?v=`,
		`href="/exocomp/assets/lift.css?v=`,
		`action="/exocomp/checks/add"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/exocomp/checks missing prefixed %q", want)
		}
	}
	for _, gone := range []string{
		`action="/checks/add"`,
		`href="/jobs"`,
		`href="/system"`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("/exocomp/checks leaked bare %q", gone)
		}
	}
	if strings.Contains(body, "/exocomp/exocomp") {
		t.Error("/exocomp/checks double-prefixed the base path")
	}
}

// TestBasePathTemplateDynamicHrefs pins the template-built dynamic sites: the
// job row links, the schedule row-action forms (rowAction.Form), the
// workspace row links, and the source row links all carry the prefix.
func TestBasePathTemplateDynamicHrefs(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	ctx := context.Background()
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	h.seedWorkspace(t, "web")
	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly", CronExpr: "0 3 * * *", Kind: "backup",
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if _, err := h.store.CreateSource(ctx, store.Source{
		Name: "fleet", Kind: store.SourcePath, URI: t.TempDir(), Enabled: true,
	}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := baseLogin(t, ts.URL, "/exocomp", h.mint(t, "laptop", "operator"))

	cases := []struct{ path, want string }{
		{"/exocomp/jobs", `href="/exocomp/jobs/` + itoa64(job.ID) + `"`},
		{"/exocomp/schedules", `action="/exocomp/schedules/nightly/disable"`},
		{"/exocomp/workspaces", `href="/exocomp/workspaces/web"`},
		{"/exocomp/sources", `href="/exocomp/sources/fleet"`},
	}
	for _, c := range cases {
		code, page := getBody(t, op, ts.URL+c.path)
		if code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", c.path, code)
		}
		if !strings.Contains(page, c.want) {
			t.Errorf("%s missing prefixed %q", c.path, c.want)
		}
		if strings.Contains(page, "/exocomp/exocomp") {
			t.Errorf("%s double-prefixed the base path", c.path)
		}
	}
}

// TestBasePathTemplateSSESeam pins the client-side stream/event seams that
// point at root-absolute endpoints from JS: the job tail and the approvals
// live list carry the base so the browser dials the prefixed route.
func TestBasePathTemplateSSESeam(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	job, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "playbook", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/sh", "-c", "sleep 30"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := baseLogin(t, ts.URL, "/exocomp", h.mint(t, "laptop", "operator"))

	for _, path := range []string{"/exocomp/jobs/" + itoa64(job.ID), "/exocomp/approvals"} {
		code, body := getBody(t, op, ts.URL+path)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, code)
		}
		if !strings.Contains(body, `data-base="/exocomp"`) {
			t.Errorf("%s missing the JS base seam", path)
		}
	}
}

// TestBasePathTemplateLeavesGoEmittedSingle proves the sweep never double-
// prefixes a spine-emitted href. The events chips (viewmodel.go) and the
// checks row Open href (row_actions_resources.go) render under exactly one
// prefix, never two.
func TestBasePathTemplateLeavesGoEmittedSingle(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	ctx := context.Background()
	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "presence", Kind: store.CheckPing, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "router", Address: "192.168.1.1"}},
	}); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if _, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", Normalized: `{"name":"presence-alert"}`,
		Outcome: store.OutcomeIgnored, Reason: "no rule matched",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := baseLogin(t, ts.URL, "/exocomp", h.mint(t, "laptop", "operator"))

	// The events all/reset chip is Go-built only (viewmodel.go SourceHref).
	code, body := getBody(t, op, ts.URL+"/exocomp/events")
	if code != http.StatusOK {
		t.Fatalf("GET /exocomp/events = %d, want 200", code)
	}
	if !strings.Contains(body, `href="/exocomp/events"`) {
		t.Error("events chip href lost its single prefix")
	}
	if strings.Contains(body, "/exocomp/exocomp") {
		t.Error("events page double-prefixed the base path")
	}
}

// TestNoBasePathTemplateDefaultUnchanged proves the default (no --base-path)
// template render is unchanged: the login action, the assets, the nav, and a
// form action all stay at the origin root.
func TestNoBasePathTemplateDefaultUnchanged(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	code, login := getBody(t, &http.Client{}, ts.URL+"/login")
	if code != http.StatusOK {
		t.Fatalf("GET /login = %d, want 200", code)
	}
	for _, want := range []string{
		`action="/login"`,
		`href="/assets/tokens.css?v=`,
		`href="/assets/app.css?v=`,
	} {
		if !strings.Contains(login, want) {
			t.Errorf("default login missing %q", want)
		}
	}

	op := baseLogin(t, ts.URL, "", h.mint(t, "laptop", "operator"))
	_, body := getBody(t, op, ts.URL+"/checks")
	for _, want := range []string{
		`href="/approvals"`,
		`href="/jobs"`,
		`href="/assets/lift.css?v=`,
		`action="/checks/add"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("default /checks missing %q", want)
		}
	}
}
