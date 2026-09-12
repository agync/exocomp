package web

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// baseLogin exchanges a token for a session on a fresh cookie-jar client that
// talks to the PREFIXED login route, the shape the browser reaches under a base
// path (origin is the listener root, prefix the surface's base path, "" at the
// root origin). The template form action is L-B2's sweep; the route itself
// lives behind the same StripPrefix, so posting to the prefixed path exercises
// the real flow.
func baseLogin(t *testing.T, origin, prefix, token string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.PostForm(origin+prefix+"/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatalf("POST %s/login: %v", origin+prefix, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	if len(jar.Cookies(mustURL(t, origin+prefix))) == 0 {
		t.Fatal("login set no session cookie")
	}
	return client
}

// TestBasePathLoginRenders is behavior (a): with --base-path /exocomp the
// prefixed /exocomp/login is served — the prefix is stripped inbound and the
// login form renders at 200.
func TestBasePathLoginRenders(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	code, body := getBody(t, &http.Client{}, ts.URL+"/exocomp/login")
	if code != http.StatusOK {
		t.Fatalf("GET /exocomp/login = %d, want 200", code)
	}
	if !strings.Contains(body, "Sign in") {
		t.Error("prefixed login page missing the sign-in form")
	}

	// The bare, unprefixed /login is not served under a base path.
	code, _ = getBody(t, &http.Client{}, ts.URL+"/login")
	if code != http.StatusNotFound {
		t.Errorf("bare /login under a base path = %d, want 404", code)
	}
}

// TestBasePathRequireAuthRedirects is behavior (b): an unauthenticated request
// to the prefixed surface bounces to the PREFIXED login with next carrying the
// prefixed request path — exactly once, never double-prefixed.
func TestBasePathRequireAuthRedirects(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/exocomp/jobs")
	if err != nil {
		t.Fatalf("GET /exocomp/jobs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauth prefixed GET = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	want := "/exocomp/login?next=%2Fexocomp%2Fjobs"
	if loc != want {
		t.Errorf("requireAuth Location = %q, want %q", loc, want)
	}
	if strings.Contains(loc, "/exocomp/exocomp") {
		t.Error("redirect double-prefixed the base path")
	}
}

// TestBasePathRedirectFlash is behavior (c): the single write chokepoint emits
// the prefixed target, so a post-write 303 lands back inside the surface.
func TestBasePathRedirectFlash(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := baseLogin(t, ts.URL, "/exocomp", h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/exocomp/checks/add", url.Values{
		"name": {"web-ping"}, "kind": {"ping"}, "interval": {"5m"}, "target": {"router=192.0.2.1"},
	}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("check add = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/exocomp/checks" {
		t.Errorf("redirectFlash Location = %q, want /exocomp/checks", loc)
	}
}

// TestBasePathGoURLSites is behavior (d): every Go-built root-absolute URL
// emits the prefixed href on the rendered page — the job detail app bar
// (handlers.go), the plan detail Plan text verb (plan_detail.go), the checks
// row Open href (row_actions_resources.go), the workspaces row Backend hrefs
// (workspaces_row.go), and the events chips (viewmodel.go).
func TestBasePathGoURLSites(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	h.seedWorkspace(t, "web")
	job, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "playbook", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := h.store.CreateCheck(context.Background(), store.NewCheck{
		Name: "presence", Kind: store.CheckPing, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "router", Address: "192.168.1.1"}},
	}); err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	if _, err := h.store.CreateEvent(context.Background(), store.Event{
		Source: "check", Normalized: `{"name":"presence-alert"}`,
		Outcome: store.OutcomeIgnored, Reason: "no rule matched",
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	art := h.seedPagePlan(t, "web", 1, `{"resources":[],"change_summary":""}`)

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := baseLogin(t, ts.URL, "/exocomp", h.mint(t, "laptop", "operator"))

	// Job detail app bar (handlers.go): BackHref and the Raw log verb.
	_, body := getBody(t, client, ts.URL+"/exocomp/jobs/"+itoa64(job.ID))
	for _, want := range []string{
		`href="/exocomp/jobs"`,
		`href="/exocomp/jobs/` + itoa64(job.ID) + `/raw"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("job detail missing prefixed href %q", want)
		}
	}

	// Plan detail (plan_detail.go): the Plan text verb opens the plan job.
	_, body = getBody(t, client, ts.URL+"/exocomp/workspaces/web/plans/"+strconv.FormatInt(art.ID, 10))
	if !strings.Contains(body, `href="/exocomp/jobs/`+itoa64(art.JobID)+`"`) {
		t.Errorf("plan detail Plan text href not prefixed (want /exocomp/jobs/%d)", art.JobID)
	}

	// Checks row (row_actions_resources.go): the Open action's events href.
	_, body = getBody(t, client, ts.URL+"/exocomp/checks")
	if !strings.Contains(body, `href="/exocomp/events?source=check"`) {
		t.Error("checks row Open href not prefixed")
	}

	// Workspaces row (workspaces_row.go): the Backend config hrefs.
	_, body = getBody(t, client, ts.URL+"/exocomp/workspaces")
	if !strings.Contains(body, `href="/exocomp/workspaces/web"`) {
		t.Error("workspace row Backend href not prefixed")
	}

	// Events chips (viewmodel.go): the all/reset chip.
	_, body = getBody(t, client, ts.URL+"/exocomp/events")
	if !strings.Contains(body, `href="/exocomp/events"`) {
		t.Error("events chip href not prefixed")
	}
}

// TestBasePathWriteRoundtrip drives a real write under the prefix end to end:
// session login, a mutating form POST, and the follow of the prefixed redirect
// landing on the prefixed /checks page with the added check rendered.
func TestBasePathWriteRoundtrip(t *testing.T) {
	h := newHarnessBase(t, "/exocomp")
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := baseLogin(t, ts.URL, "/exocomp", h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/exocomp/checks/add", url.Values{
		"name": {"web-ping"}, "kind": {"ping"}, "interval": {"5m"}, "target": {"router=192.0.2.1"},
	}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("check add = %d, want 303", resp.StatusCode)
	}
	code, body := getBody(t, client, ts.URL+"/exocomp/checks")
	if code != http.StatusOK {
		t.Fatalf("prefixed /checks after redirect = %d, want 200", code)
	}
	if !strings.Contains(body, "web-ping") {
		t.Error("the added check does not render on the prefixed page")
	}
}

// TestNoBasePathDefaultUnchanged proves the default (no --base-path) behavior
// is unchanged: unprefixed requireAuth bounce, unprefixed flash target,
// unprefixed Go URL sites.
func TestNoBasePathDefaultUnchanged(t *testing.T) {
	h := newHarness(t)
	job, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "playbook", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// requireAuth bounce stays at the origin root.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/jobs")
	if err != nil {
		t.Fatalf("GET /jobs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if loc := resp.Header.Get("Location"); loc != "/login?next=%2Fjobs" {
		t.Errorf("requireAuth Location = %q, want /login?next=%%2Fjobs", loc)
	}

	// A write still flashes to the root path.
	op := baseLogin(t, ts.URL, "", h.mint(t, "laptop", "operator"))
	resp = postForm(t, op, ts.URL+"/checks/add", url.Values{
		"name": {"web-ping"}, "kind": {"ping"}, "interval": {"5m"}, "target": {"router=192.0.2.1"},
	}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("check add = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/checks" {
		t.Errorf("redirectFlash Location = %q, want /checks", loc)
	}

	// Go URL sites stay unprefixed.
	_, body := getBody(t, op, ts.URL+"/jobs/"+itoa64(job.ID))
	if !strings.Contains(body, `href="/jobs/`+itoa64(job.ID)+`/raw"`) {
		t.Error("job detail Raw log href changed under the default")
	}
}
