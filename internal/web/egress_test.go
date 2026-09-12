package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestAPIPlaybookRunCarriesEgressPorts: an operator POSTing egress_ports lands
// them on the enqueued job row, the seam that makes the sandboxed daemon grant
// the run its nonstandard inventory port. Dropping the field from apiPlaybookRun
// fails this test.
func TestAPIPlaybookRunCarriesEgressPorts(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token,
		`{"playbook":"/srv/site.yml","egress_ports":[2222,22,2222]}`, nil)
	if code != http.StatusOK {
		t.Fatalf("run status = %d, want 200 (%s)", code, body)
	}
	_, _, data := decodeEnvelope(t, body)
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil || jv.JobID == 0 {
		t.Fatalf("no job_id: %v (%s)", err, body)
	}
	job, err := h.store.GetJob(context.Background(), jv.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !slices.Equal(job.Args.EgressPorts, []int{22, 2222}) {
		t.Fatalf("job EgressPorts = %v, want [22 2222] (deduped, sorted)", job.Args.EgressPorts)
	}
}

// TestAPIPlaybookRunRefusesJunkEgress: an out-of-range port in egress_ports is
// a 400 before anything is enqueued — junk is refused at the write face, never
// silently dropped.
func TestAPIPlaybookRunRefusesJunkEgress(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	for _, egress := range []string{"[0]", "[70000]", `["abc"]`} {
		code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token,
			`{"playbook":"/srv/site.yml","egress_ports":`+egress+`}`, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("egress_ports=%s status = %d, want 400 (%s)", egress, code, body)
		}
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a junk-egress run still created %d job(s)", len(jobs))
	}
}

// TestAPIPlaybookRunEgressReadonlyForbidden: a readonly token is refused even
// when the body carries egress_ports — the route's operator-scope gate holds,
// so an agent that cannot enqueue writes cannot widen egress either.
func TestAPIPlaybookRunEgressReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", ro,
		`{"playbook":"/srv/site.yml","egress_ports":[2222]}`, nil)
	if code != http.StatusForbidden {
		t.Fatalf("readonly run status = %d, want 403 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != "forbidden" {
		t.Fatalf("readonly run envelope = %s, want error/forbidden", body)
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused readonly run created %d job(s)", len(jobs))
	}
}

// TestSourceRunPlaybookCarriesEgress: the launch drawer's egress field reaches
// the enqueued job row, so a browser operator grants a nonstandard inventory
// port the same way the CLI does.
func TestSourceRunPlaybookCarriesEgress(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
		url.Values{"playbook": {"site.yml"}, "egress": {"2222"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("playbook run = %d, want 303", resp.StatusCode)
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || !slices.Equal(jobs[0].Args.EgressPorts, []int{2222}) {
		t.Fatalf("egress run did not mark the row: %+v", jobs)
	}
}

// TestSourceRunPlaybookRefusesJunkEgress: a junk port on the launcher form is
// a 422 with the egress field's refusal, the typed value kept, and no job is
// created — the forms pattern, never a bare 400.
func TestSourceRunPlaybookRefusesJunkEgress(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	for _, egress := range []string{"0", "70000", "abc"} {
		resp := postForm(t, client, ts.URL+"/sources/"+name+"/run/playbook",
			url.Values{"playbook": {"site.yml"}, "egress": {egress}}, nil)
		body := readBody(t, resp)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("egress=%s run = %d, want 422 (%s)", egress, resp.StatusCode, body)
		}
		if !strings.Contains(body, `id="rp-egress-meta" class="meta is-error"`) ||
			!strings.Contains(body, "Egress ports are numbers from 1 to 65535") {
			t.Errorf("egress=%s refusal missing the field's is-error meta:\n%s", egress, body)
		}
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a junk-egress source run created %d job(s)", len(jobs))
	}
}

// TestSourceLaunchDrawerRendersEgressField: the playbook launcher offers the
// egress input an operator fills in to reach a nonstandard-port host.
func TestSourceLaunchDrawerRendersEgressField(t *testing.T) {
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
	if !strings.Contains(page, `name="egress"`) {
		t.Errorf("launch drawer missing the egress input:\n%s", page)
	}
}

// TestJobRerunPreservesEgressPorts: a rerun clones the original job's egress
// grant onto the new spec, so re-running a playbook that reached a
// nonstandard port does not silently drop it.
func TestJobRerunPreservesEgressPorts(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	orig := seedTerminalJob(t, h, store.JobArgs{
		Argv:        []string{"/bin/true"},
		EgressPorts: []int{2222},
	})

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	resp := postForm(t, client, ts.URL+"/jobs/"+itoa64(orig.ID)+"/rerun", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rerun = %d, want 303", resp.StatusCode)
	}

	got := rerunJob(t, h)
	if !slices.Equal(got.Args.EgressPorts, []int{2222}) {
		t.Fatalf("rerun EgressPorts = %v, want [2222]", got.Args.EgressPorts)
	}
}
