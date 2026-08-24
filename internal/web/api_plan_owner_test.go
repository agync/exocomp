package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// TestAPIApplyRefusesForeignPlan proves a plan cannot be applied against a
// workspace other than the one that produced it: the workspace came from
// the path and the plan from the body, checked only for its artifact KIND, so
// one workspace's sealed plan could be applied inside another — under that
// other workspace's backend bearer and state passphrase, and with the stale-plan
// serial read from a workspace the plan was never made against. A plan is now
// refused unless the workspace in the path is the one that produced it.
func TestAPIApplyRefusesForeignPlan(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	token := h.mint(t, "op", "operator")
	h.seedWorkspace(t, "prod")
	h.seedWorkspace(t, "sandbox")
	art := h.seedPlanArtifact(t, "prod")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/sandbox/apply", token,
		fmt.Sprintf(`{"plan":"%d"}`, art.ID), nil)
	if code != http.StatusBadRequest {
		t.Fatalf("foreign-plan apply status = %d, want 400 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != codeInvalidArgument {
		t.Fatalf("foreign-plan envelope = %s, want error/invalid_argument", body)
	}
	jobs, err := h.store.ListJobs(ctx, store.JobFilter{Runbook: service.WorkspaceRunbook("sandbox")})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a foreign plan enqueued %d job(s) in sandbox", len(jobs))
	}

	// Its own workspace still applies it.
	code, _, body = bearerPost(t, ts.URL, "/api/v1/workspaces/prod/apply", token,
		fmt.Sprintf(`{"plan":"%d"}`, art.ID), nil)
	if code != http.StatusOK {
		t.Fatalf("own-workspace apply status = %d, want 200 (%s)", code, body)
	}
	var jv jobEnqueuedView
	decodeData(t, body, &jv)
	job, err := h.store.GetJob(ctx, jv.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Runbook != service.WorkspaceRunbook("prod") ||
		!strings.Contains(strings.Join(job.Args.Argv, " "), art.Path) {
		t.Fatalf("apply job = %q %v, want prod applying its own sealed plan", job.Runbook, job.Args.Argv)
	}
}
