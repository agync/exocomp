package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// sealedPlanApproval seals planJSON into an artifact and binds a pending
// approval to it, the shape an approval carrying a plan diff has on disk.
func (h *harness) sealedPlanApproval(t *testing.T, planJSON string) store.Approval {
	t.Helper()
	ctx := context.Background()

	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "tofu", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue plan job: %v", err)
	}

	plain := filepath.Join(h.dir, "plan-"+itoa64(job.ID)+".json")
	if err := os.WriteFile(plain, []byte(planJSON), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	sealed, err := h.env.SealFile(plain)
	if err != nil {
		t.Fatalf("SealFile: %v", err)
	}
	art, err := h.store.CreateArtifact(ctx, store.Artifact{
		JobID: job.ID, Kind: reactive.PlanArtifactKind, Path: sealed, SHA256: "x", Size: 1,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	_, hash, err := reactive.ApprovalToken(h.env, job.ID)
	if err != nil {
		t.Fatalf("ApprovalToken: %v", err)
	}
	now := time.Now().UTC()
	approval, err := h.store.CreateApproval(ctx, store.Approval{
		JobID: job.ID, TokenHash: hash, RequestedAt: now, ExpiresAt: now.Add(time.Hour),
		PlanArtifact: &art.ID,
	})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	return approval
}

// TestAPIApprovalPlan covers the raw-plan-text endpoint: unauthenticated is 401,
// the readonly scope is forbidden (the plan diff sits behind the plan-view floor
// — operator or approve, the same the browser surface renders it behind), and an
// operator gets the sealed plan opened as raw text through the SAME OpenSealedFile
// path the web approval surface uses.
func TestAPIApprovalPlan(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	const planJSON = `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["create"]}}]}`
	approval := h.sealedPlanApproval(t, planJSON)
	path := "/api/v1/approvals/" + itoa64(approval.ID) + "/plan"

	// (a) unauthenticated -> 401
	if code, _, _ := bearerGet(t, ts.URL, path, ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated plan status = %d, want 401", code)
	}

	// (b) wrong scope: readonly may read most surfaces, but not the plan diff.
	ro := h.mint(t, "ro", "readonly")
	code, _, body := bearerGet(t, ts.URL, path, ro)
	if code != http.StatusForbidden {
		t.Fatalf("readonly plan status = %d, want 403 (%s)", code, body)
	}
	if _, ecode, _ := decodeEnvelope(t, body); ecode != "forbidden" {
		t.Fatalf("readonly plan error code = %q, want forbidden (%s)", ecode, body)
	}

	// (c) happy path: an operator reads the plan back as raw text.
	op := h.mint(t, "op", "operator")
	code, _, body = bearerGet(t, ts.URL, path, op)
	if code != http.StatusOK {
		t.Fatalf("operator plan status = %d, want 200 (%s)", code, body)
	}
	kind, _, _ := decodeEnvelope(t, body)
	if kind != kindApprovalPlan {
		t.Fatalf("plan kind = %q, want %q (%s)", kind, kindApprovalPlan, body)
	}
	var v approvalPlanView
	decodeData(t, body, &v)
	if v.Plan != planJSON {
		t.Fatalf("plan text = %q, want the sealed plan opened verbatim", v.Plan)
	}
}

// TestAPIApprovalPlanNoPlan proves an approval with no plan artifact is a 404,
// not an empty success — matching what the web path shows when there is nothing
// to render.
func TestAPIApprovalPlanNoPlan(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	_, approval := h.gatedJob(t, time.Hour) // gatedJob binds no plan artifact
	op := h.mint(t, "op", "operator")
	path := "/api/v1/approvals/" + itoa64(approval.ID) + "/plan"
	code, _, body := bearerGet(t, ts.URL, path, op)
	if code != http.StatusNotFound {
		t.Fatalf("no-plan status = %d, want 404 (%s)", code, body)
	}
}
