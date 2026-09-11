package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// ---- Plan detail diff shapes (SPEC §4.1) ----

// seedPagePlan writes a REAL sealed plan for a workspace so the plan-detail
// page can open and classify it — the same shape seedPlanDiff uses in
// workspace_write_test.go under a distinct name, because both live in the web
// test module.
func (h *harness) seedPagePlan(t *testing.T, ws string, planSerial int64, planJSON string) store.Artifact {
	t.Helper()
	ctx := context.Background()
	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: service.WorkspaceRunbook(ws), Kind: "tofu", Trigger: "manual",
		Argv: []string{"/bin/true"},
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
		JobID: job.ID, Kind: reactive.PlanArtifactKind, Path: sealed,
		SHA256: fmt.Sprintf("diff-%d", job.ID), Size: 1,
		PlanSerial: planSerial,
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	return art
}

// planDiffBody renders the plan-detail page for a seeded plan as an operator.
func planDiffBody(t *testing.T, h *harness, art store.Artifact, query string) string {
	t.Helper()
	h.seedWorkspace(t, "web")
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	code, body := getBody(t, client, ts.URL+"/workspaces/web/plans/"+strconv.FormatInt(art.ID, 10)+query)
	if code != http.StatusOK {
		t.Fatalf("GET plan detail = %d (%s)", code, body)
	}
	return body
}

// TestPlanDetailInlineShape renders the inline value cell (SPEC §4.1): the old
// value in the .old span, the arrow, the new value in the .new span, the
// display renderer quoted. The literal word "continue" never appears anywhere
// in the rendered body (B1) — the template loops the changed set only, so no
// loop-body skip exists to render.
func TestPlanDetailInlineShape(t *testing.T) {
	h := newHarness(t)
	art := h.seedPagePlan(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],
			"before":{"data":{"marker":"DRIFTED"},"keep":"k"},"after":{"data":{"marker":"hello-exocomp"},"keep":"k"}}}
	]}`)
	body := planDiffBody(t, h, art, "")
	for _, want := range []string{
		`class="old">&#34;DRIFTED&#34;<`,
		`class="arrow"`,
		`class="new">&#34;hello-exocomp&#34;<`,
		`class="count-chg">1 changed<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inline shape missing %q", want)
		}
	}
	if strings.Contains(body, "continue") {
		t.Error("rendered body contains the literal word continue (B1)")
	}
}

// TestPlanDetailBlockShape renders a scalar too long for the inline cell as two
// full-width block lines: − old on the fail tint, + new on the ok tint, the
// sign in the first grid column and the value in the remaining width.
func TestPlanDetailBlockShape(t *testing.T) {
	h := newHarness(t)
	art := h.seedPagePlan(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],
			"before":{"data":{"notes":"provisioned by exocomp job #2 against serial 0 · lima k3s · owner platform-team · do not edit by hand"}},
			"after":{"data":{"notes":"provisioned by exocomp job #4 against serial 1 · lima k3s · owner platform-team · do not edit by hand · drift restored 19:30"}}}}
	]}`)
	body := planDiffBody(t, h, art, "")
	for _, want := range []string{
		`class="block">`,
		`class="ln del"><span aria-hidden="true">−</span><span>`,
		`class="ln add"><span aria-hidden="true">+</span><span>`,
		`&#34;provisioned by exocomp job #2`,
		`drift restored 19:30&#34;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("block shape missing %q", want)
		}
	}
}

// TestPlanDetailObjectShape renders an object attr as the indented pre: the
// server-set key shows its placeholder, the N of K keys changed · raw footer
// sits under the pre, and a redacted resource shows the secret marker. The
// redacted object's raw pair (the engine keeps the plaintext minified JSON on
// RawBefore/RawAfter) must never render (invariant 4).
func TestPlanDetailObjectShape(t *testing.T) {
	h := newHarness(t)
	art := h.seedPagePlan(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],
			"before":{"metadata":[{"a":1,"labels":{},"name":"e2e-marker","uid":"u1"}]},
			"after":{"metadata":[{"a":2,"labels":{"owner":"platform-team"},"name":"e2e-marker","uid":"u1"}]}}},
		{"address":"kubernetes_secret.gone","change":{"actions":["delete"],
			"before":{"token":"hunter2"},"before_sensitive":{"token":true}}}
	]}`)
	body := planDiffBody(t, h, art, "")
	for _, want := range []string{
		`class="obj">`,
		"[redacted · server-set]",
		"2 of 3 keys changed",
		"<summary>raw</summary>",
		"[redacted · secret]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("object shape missing %q", want)
		}
	}
	// The redacted resource's values never carry their plaintext, exactly as
	// the phone and /api/v1 promise.
	if strings.Contains(body, "hunter2") {
		t.Error("plan detail leaked a secret-backed value")
	}
}

// TestPlanDetailUnchangedFooter renders the unchanged collapse (SPEC §4.1): the
// row cell and the footer count the SAME flattened lists (B3) — 1 changed · 4
// same in the row, "4 attributes unchanged · show" under the hairline — and the
// expanded list shows each unchanged attr's display value, an object collapsed
// to "1 block · N keys · unchanged", an empty object as {}.
func TestPlanDetailUnchangedFooter(t *testing.T) {
	h := newHarness(t)
	art := h.seedPagePlan(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],
			"before":{"data":{"marker":"DRIFTED"},"binary_data":{},"id":"exocomp-e2e/e2e-marker","immutable":false,"metadata":{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7}},
			"after":{"data":{"marker":"hello-exocomp"},"binary_data":{},"id":"exocomp-e2e/e2e-marker","immutable":false,"metadata":{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7}}}}
	]}`)
	body := planDiffBody(t, h, art, "")
	for _, want := range []string{
		`>1 changed</span> · 4 same<`,
		`class="same-row"`,
		"4 attributes unchanged · ",
		`>show<`,
		`class="v">{}</div>`,
		`>1 block · 7 keys · unchanged<`,
		`>&#34;exocomp-e2e/e2e-marker&#34;<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unchanged footer missing %q", want)
		}
	}
}

// TestPlanDetailNoOpRow: a no-op resource renders the single word "unchanged"
// in its count cell with no attribute grid — never "0 changed · N same" (B3).
func TestPlanDetailNoOpRow(t *testing.T) {
	h := newHarness(t)
	art := h.seedPagePlan(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_namespace.e2e","change":{"actions":["no-op"],"before":{"a":1},"after":{"a":1}}}
	]}`)
	body := planDiffBody(t, h, art, "")
	if !strings.Contains(body, `class="unchanged">unchanged<`) {
		t.Error("no-op row did not render the unchanged word")
	}
	if strings.Contains(body, "0 changed") {
		t.Error("no-op row rendered a changed count")
	}
	if strings.Contains(body, `class="attrs"`) {
		t.Error("no-op row rendered an attribute grid")
	}
}

// TestPlanDetailIdenticalMapsAgree (B2): a resource whose only diff is
// identical nested maps under a different key order renders NO changed attr
// rows — the counts in the row cell and the footer both come from the same
// flattened lists and agree.
func TestPlanDetailIdenticalMapsAgree(t *testing.T) {
	h := newHarness(t)
	art := h.seedPagePlan(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],
			"before":{"marker":{"a":1,"b":2}},"after":{"marker":{"b":2,"a":1}}}}
	]}`)
	body := planDiffBody(t, h, art, "")
	for _, want := range []string{
		`class="count-chg">0 changed<`,
		` · 1 same<`,
		"1 attributes unchanged · ",
		`>1 block · 2 keys · unchanged<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("identical-maps row missing %q", want)
		}
	}
	if strings.Contains(body, `class="old"`) {
		t.Error("identical maps rendered a changed attr row")
	}
}

// TestPlanDetailConfirmCapsAttrs: the confirm dialog repeats the diff grid but
// caps each resource at 3 attribute rows then "N more →" (SPEC §4.1). The full
// page diff shows all four rows; the dialog's own slice shows the cap.
func TestPlanDetailConfirmCapsAttrs(t *testing.T) {
	h := newHarness(t)
	art := h.seedPagePlan(t, "web", 1, `{"resource_changes":[
		{"address":"kubernetes_config_map.marker","change":{"actions":["update"],
			"before":{"a":"old-a","b":"old-b","c":"old-c","d":"old-d"},
			"after":{"a":"new-a","b":"new-b","c":"new-c","d":"new-d"}}}
	]}`)
	body := planDiffBody(t, h, art, "?launch=1")
	di := strings.Index(body, `id="plan-confirm"`)
	if di < 0 {
		t.Fatal("no confirm dialog rendered")
	}
	dlg := body[di:]
	dlg = dlg[:strings.Index(dlg, "</dialog>")]
	for _, want := range []string{
		`<span class="k">a</span>`,
		`<span class="k">b</span>`,
		`<span class="k">c</span>`,
		`>1 more →<`,
	} {
		if !strings.Contains(dlg, want) {
			t.Errorf("confirm dialog missing %q", want)
		}
	}
	if strings.Contains(dlg, `<span class="k">d</span>`) {
		t.Error("confirm dialog shows past the 3-row cap")
	}
}
