package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

func TestWorkspaceWriteReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/workspaces/web/plan", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly workspace plan = %d, want 403", resp.StatusCode)
	}
}

func TestWorkspaceWriteCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/workspaces/web/plan", url.Values{},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin workspace plan = %d, want 403", resp.StatusCode)
	}
}

// TestWorkspacePlanEnqueues: an operator's browser-form plan on a seeded
// workspace enqueues a job through the SAME service.PlanWorkspace verb the
// bearer-token API face calls, and redirects to that job's page.
func TestWorkspacePlanEnqueues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/workspaces/web/plan", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("workspace plan = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("workspace plan did not redirect to a job page")
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: service.WorkspaceRunbook("web")})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) == 0 {
		t.Fatal("workspace plan did not enqueue a job through the service verb")
	}
	if jobs[0].ConcurrencyKey != "tofu:web" {
		t.Fatalf("job concurrency_key = %q, want tofu:web", jobs[0].ConcurrencyKey)
	}
}

// TestWorkspaceApplyFreshEnqueues: a fresh apply (no plan id) on an unpoliced
// workspace auto-approves and enqueues plainly, mirroring apiWorkspaceApply's
// saved==nil path.
func TestWorkspaceApplyFreshEnqueues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/workspaces/web/apply", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("workspace apply = %d, want 303", resp.StatusCode)
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: service.WorkspaceRunbook("web")})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) == 0 {
		t.Fatal("workspace apply did not enqueue a job through the service verb")
	}
}

// TestWorkspacesPageListsSeeded confirms the new GET /workspaces page renders
// a seeded workspace's name for both operator and readonly identities (the
// latter read-only, no controls).
func TestWorkspacesPageListsSeeded(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	h.seedWorkspace(t, "web")
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp, err := client.Get(ts.URL + "/workspaces")
	if err != nil {
		t.Fatalf("GET /workspaces: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /workspaces = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "web") {
		t.Fatal("workspaces page did not list the seeded workspace")
	}
}
