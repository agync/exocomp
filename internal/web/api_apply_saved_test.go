package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// TestAPIApplyPlainSavedPlanEnqueues: a saved plan that carries NO policy verdict
// applies by id — it enqueues an `apply <sealed-plan>` (the daemon decrypts the
// sealed path on claim), rather than being refused. The row's argv names the
// sealed plan, proving the write routed the saved plan through the queue.
func TestAPIApplyPlainSavedPlanEnqueues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	ctx := context.Background()
	h.seedWorkspace(t, "web")

	planJob, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook("web"), Kind: "tofu", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue plan job: %v", err)
	}
	art, err := h.store.CreateArtifact(ctx, store.Artifact{
		JobID: planJob.ID, Kind: reactive.PlanArtifactKind,
		Path: "/data/artifacts/tofu/web-9.tfplan.age", SHA256: "x", Size: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	code, _, body := bearerPost(t, ts.URL, "/api/v1/workspaces/web/apply", token,
		fmt.Sprintf(`{"plan":"%d"}`, art.ID), nil)
	if code != http.StatusOK {
		t.Fatalf("plain saved-plan apply status = %d, want 200 (%s)", code, body)
	}
	kind, _, data := decodeEnvelope(t, body)
	if kind != kindJobEnqueued {
		t.Fatalf("kind = %q, want %q (%s)", kind, kindJobEnqueued, body)
	}
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil || jv.JobID == 0 {
		t.Fatalf("no job_id: %v (%s)", err, body)
	}
	job, err := h.store.GetJob(ctx, jv.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !strings.Contains(strings.Join(job.Args.Argv, " "), art.Path) {
		t.Errorf("apply argv = %v, want the sealed plan path %q", job.Args.Argv, art.Path)
	}
}
