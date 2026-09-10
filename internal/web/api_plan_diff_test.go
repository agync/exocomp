package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
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

// TestAPIApprovalPlan covers the structured plan-diff endpoint's scope gate and
// shape: unauthenticated is 401, the readonly scope is forbidden (the plan diff
// sits behind the plan-view floor — operator or approve, the same the browser
// surface renders it behind), and an operator gets the classified diff through
// the SAME OpenSealedFile + classifyPlan path the web approval surface uses.
func TestAPIApprovalPlan(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	const planJSON = `{"resource_changes":[{"address":"null_resource.x","change":{"actions":["create"],"after":{"triggers":"go"}}}]}`
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

	// (c) happy path: an operator reads the structured, classified diff.
	op := h.mint(t, "op", "operator")
	code, _, body = bearerGet(t, ts.URL, path, op)
	if code != http.StatusOK {
		t.Fatalf("operator plan status = %d, want 200 (%s)", code, body)
	}
	kind, _, _ := decodeEnvelope(t, body)
	if kind != wire.KindApprovalPlan {
		t.Fatalf("plan kind = %q, want %q (%s)", kind, wire.KindApprovalPlan, body)
	}
	var v wire.ApprovalPlanView
	decodeData(t, body, &v)
	if len(v.Creates) != 1 || v.Creates[0].Address != "null_resource.x" {
		t.Fatalf("creates = %+v, want the one created resource", v.Creates)
	}
	if v.LineCount != 1 {
		t.Fatalf("line_count = %d, want 1", v.LineCount)
	}
	if v.ArtifactID != *approval.PlanArtifact {
		t.Fatalf("artifact_id = %d, want %d", v.ArtifactID, *approval.PlanArtifact)
	}
	if v.Summary == "" {
		t.Fatalf("summary is empty, want a counted summary")
	}
}

// TestAPIApprovalPlanRedaction is the security gate: a plan whose classification
// includes a secret-backed attribute must reach the phone with that value
// redacted exactly as the web renders it — never its plaintext — while an
// ordinary attribute keeps its value. The sensitive attribute is seeded the way
// the web classifier reads sensitivity: an `after_sensitive` tree marking the
// attribute true (renderPlan's hasSensitive), so the parity is real.
func TestAPIApprovalPlanRedaction(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// Three resources: a delete (destroy bucket), a redacted update whose
	// after_sensitive flags `password`, and a plain create whose value shows.
	const planJSON = `{"resource_changes":[
		{"address":"null_resource.gone","change":{"actions":["delete"]}},
		{"address":"vault_secret.db","change":{"actions":["update"],"after":{"password":"hunter2","name":"db"},"after_sensitive":{"password":true}}},
		{"address":"null_resource.pub","change":{"actions":["create"],"after":{"region":"eu-west-1"}}}
	]}`
	approval := h.sealedPlanApproval(t, planJSON)
	path := "/api/v1/approvals/" + itoa64(approval.ID) + "/plan"

	op := h.mint(t, "op", "operator")
	code, _, body := bearerGet(t, ts.URL, path, op)
	if code != http.StatusOK {
		t.Fatalf("operator plan status = %d, want 200 (%s)", code, body)
	}

	// (a) the structure is destroys / changes / creates / summary.
	var v wire.ApprovalPlanView
	decodeData(t, body, &v)
	if len(v.Destroys) != 1 || v.Destroys[0].Address != "null_resource.gone" {
		t.Fatalf("destroys = %+v, want the one deleted resource", v.Destroys)
	}
	if len(v.Changes) != 1 || v.Changes[0].Address != "vault_secret.db" {
		t.Fatalf("changes = %+v, want the one updated resource", v.Changes)
	}
	if len(v.Creates) != 1 || v.Creates[0].Address != "null_resource.pub" {
		t.Fatalf("creates = %+v, want the one created resource", v.Creates)
	}
	if v.Summary == "" {
		t.Fatalf("summary is empty, want a counted summary")
	}

	// (b) destroys come first in the wire bytes, before changes and creates.
	di, ci, cr := strings.Index(body, `"destroys"`), strings.Index(body, `"changes"`), strings.Index(body, `"creates"`)
	if di < 0 || di >= ci || ci >= cr {
		t.Fatalf("key order destroys(%d) changes(%d) creates(%d), want destroys first", di, ci, cr)
	}

	// (c) the sensitive attribute's PLAINTEXT never appears anywhere in the JSON.
	if strings.Contains(body, "hunter2") {
		t.Fatalf("secret value leaked into the plan diff: %s", body)
	}
	// The redacted resource is flagged and its value is the web's marker.
	upd := v.Changes[0]
	if !upd.Redacted {
		t.Fatalf("updated resource with a sensitive attr is not flagged redacted: %+v", upd)
	}
	var pw *wire.PlanAttrView
	for i := range upd.Attrs {
		if upd.Attrs[i].Name == "password" {
			pw = &upd.Attrs[i]
		}
	}
	if pw == nil || pw.Value != redactedValue || !pw.Redacted {
		t.Fatalf("password attr = %+v, want value %q and redacted", pw, redactedValue)
	}

	// (d) a non-sensitive attribute keeps its value.
	if !strings.Contains(body, "eu-west-1") {
		t.Fatalf("non-sensitive value was dropped from the plan diff: %s", body)
	}
	crt := v.Creates[0]
	if len(crt.Attrs) != 1 || crt.Attrs[0].Name != "region" || crt.Attrs[0].Value != "eu-west-1" || crt.Attrs[0].Redacted {
		t.Fatalf("region attr = %+v, want it shown verbatim", crt.Attrs)
	}
}

// TestAPIApprovalPlanBeforeAfter: the classifier now emits the old value for
// every attribute (SPEC §4), so the plan-detail's old→new strike is possible.
// The API projection carries Before alongside the post-apply Value: "old" for
// a changed attribute, empty for one that is newly created. The union key set
// means an attribute present only in `after` (a creation) has an empty Before.
func TestAPIApprovalPlanBeforeAfter(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	const planJSON = `{"resource_changes":[
		{"address":"null_resource.cfg","change":{"actions":["update"],"before":{"tag":"old"},"after":{"tag":"new"}}},
		{"address":"null_resource.fresh","change":{"actions":["create"],"after":{"name":"x"}}}
	]}`
	approval := h.sealedPlanApproval(t, planJSON)
	path := "/api/v1/approvals/" + itoa64(approval.ID) + "/plan"

	op := h.mint(t, "op", "operator")
	code, _, body := bearerGet(t, ts.URL, path, op)
	if code != http.StatusOK {
		t.Fatalf("operator plan status = %d, want 200 (%s)", code, body)
	}
	var v wire.ApprovalPlanView
	decodeData(t, body, &v)
	if len(v.Changes) != 1 || len(v.Creates) != 1 {
		t.Fatalf("changes/creates = %d/%d, want 1/1 (%s)", len(v.Changes), len(v.Creates), body)
	}
	upd := v.Changes[0]
	if len(upd.Attrs) != 1 || upd.Attrs[0].Name != "tag" || upd.Attrs[0].Before != "old" || upd.Attrs[0].Value != "new" {
		t.Fatalf("updated attr = %+v, want tag old→new", upd.Attrs)
	}
	crt := v.Creates[0]
	if len(crt.Attrs) != 1 || crt.Attrs[0].Name != "name" || crt.Attrs[0].Before != "" || crt.Attrs[0].Value != "x" {
		t.Fatalf("created attr = %+v, want name (empty before) → x", crt.Attrs)
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
