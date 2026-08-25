package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// TestAPIPlaybookRunEnqueuesThroughService: an operator POSTing a playbook run
// gets a {job_id} envelope, and the row carries the ansible runbook/kind and the
// operator flags ONLY the service verb's spec assembly sets — proof the write
// went through internal/service and the queue, never a direct mutation.
func TestAPIPlaybookRunEnqueuesThroughService(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	code, hdr, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token,
		`{"playbook":"/srv/site.yml","limit":"web","tolerate_unreachable":true,"extra_vars":["proof=xyz"]}`, nil)
	if code != http.StatusOK {
		t.Fatalf("run status = %d, want 200 (%s)", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	kind, _, data := decodeEnvelope(t, body)
	if kind != kindJobEnqueued {
		t.Fatalf("kind = %q, want %q (%s)", kind, kindJobEnqueued, body)
	}
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil || jv.JobID == 0 {
		t.Fatalf("no job_id: %v (%s)", err, body)
	}

	job, err := h.store.GetJob(context.Background(), jv.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Runbook != "playbook" || job.Kind != executor.KindAnsible {
		t.Fatalf("job header = %q/%q, want playbook/ansible", job.Runbook, job.Kind)
	}
	if !job.Args.TolerateUnreachable {
		t.Error("tolerate_unreachable not carried onto the row")
	}
	if job.Args.ExtraVars["proof"] != "xyz" {
		t.Errorf("extra-vars lost: %+v", job.Args.ExtraVars)
	}
	argv := strings.Join(job.Args.Argv, " ")
	if !strings.Contains(argv, "--limit web") || !strings.Contains(argv, "/srv/site.yml") {
		t.Errorf("argv = %v, want --limit web and the playbook", job.Args.Argv)
	}
	// A key=val extra-var never rides ansible's argv (it is written to a run file).
	if strings.Contains(argv, "proof") {
		t.Errorf("extra-var value leaked onto argv: %v", job.Args.Argv)
	}
}

// TestAPIPlaybookRunSourceCarriesCheckoutDir: a --source run over the API must
// set the job's working Dir to the source checkout so the checkout's
// ./ansible.cfg (relative inventory/roles) is honored — the API path once set
// Source without Dir, so the run fell back to the scratch dir and read no config.
func TestAPIPlaybookRunSourceCarriesCheckoutDir(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "playbooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "playbooks", "site.yml"), []byte("- hosts: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateSource(context.Background(), store.Source{
		Name: "fleet", Kind: store.SourcePath, URI: root, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token,
		`{"source":"fleet","playbook":"playbooks/site.yml","check":true}`, nil)
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
	if job.Args.Dir != root {
		t.Errorf("job Dir = %q, want checkout root %q", job.Args.Dir, root)
	}
	if job.Args.Source != "fleet" {
		t.Errorf("job Source = %q, want fleet", job.Args.Source)
	}
}

// TestAPIPlaybookRunCarriesRunnerName: a run naming a runner sets
// JobArgs.RunnerName on the enqueued row, the same seam workspace plan/apply use.
func TestAPIPlaybookRunCarriesRunnerName(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token,
		`{"playbook":"/srv/site.yml","runner":"fedora-ee"}`, nil)
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
	if job.Args.RunnerName != "fedora-ee" {
		t.Fatalf("job.Args.RunnerName = %q, want fedora-ee", job.Args.RunnerName)
	}
}

// TestAPIPlaybookRunInventoryByName: a run naming a registered inventory resolves
// it server-side to its path (in argv) and records the name on the row so a
// dynamic inventory's env_binding resolves when the daemon runs it.
func TestAPIPlaybookRunInventoryByName(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	ctx := context.Background()

	invPath := t.TempDir() + "/hosts.ini"
	if _, err := h.store.CreateInventory(ctx, store.Inventory{
		Name: "fleet", Path: invPath, Kind: store.InventoryStatic,
	}); err != nil {
		t.Fatalf("CreateInventory: %v", err)
	}

	code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token,
		`{"playbook":"/srv/site.yml","inventory":"fleet"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("run status = %d, want 200 (%s)", code, body)
	}
	_, _, data := decodeEnvelope(t, body)
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil {
		t.Fatalf("data: %v", err)
	}
	job, err := h.store.GetJob(ctx, jv.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Args.Inventory != "fleet" {
		t.Errorf("inventory name = %q, want fleet (env-binding resolution needs it)", job.Args.Inventory)
	}
	if !strings.Contains(strings.Join(job.Args.Argv, " "), invPath) {
		t.Errorf("argv missing resolved inventory path %q: %v", invPath, job.Args.Argv)
	}
}

// TestAPIPlaybookRunScopeGate: a readonly token is forbidden and no token is
// unauthenticated, both before any job is created.
func TestAPIPlaybookRunScopeGate(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	ro := h.mint(t, "ro", "readonly")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", ro, `{"playbook":"/srv/site.yml"}`, nil)
	if code != http.StatusForbidden {
		t.Fatalf("readonly run status = %d, want 403 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != "forbidden" {
		t.Fatalf("readonly run envelope = %s, want error/forbidden", body)
	}

	code, _, body = bearerPost(t, ts.URL, "/api/v1/playbooks/run", "", `{"playbook":"/srv/site.yml"}`, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token run status = %d, want 401 (%s)", code, body)
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused run still created %d job(s)", len(jobs))
	}
}

// TestAPIPlaybookRunRequiresPlaybook: a run with no playbook is a 400.
func TestAPIPlaybookRunRequiresPlaybook(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token, `{}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("empty run status = %d, want 400 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != codeInvalidArgument {
		t.Fatalf("empty run envelope = %s, want error/invalid_argument", body)
	}
}

// TestAPIPlaybookRunIdempotencyKey: two runs with the same Idempotency-Key dedup
// to one job.
func TestAPIPlaybookRunIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	hdr := map[string]string{"Idempotency-Key": "run-1"}

	first := postRunJobID(t, ts.URL, token, hdr)
	second := postRunJobID(t, ts.URL, token, hdr)
	if first != second {
		t.Fatalf("idempotency-key produced two jobs: %d and %d", first, second)
	}
}

func postRunJobID(t *testing.T, base, token string, hdr map[string]string) int64 {
	t.Helper()
	code, _, body := bearerPost(t, base, "/api/v1/playbooks/run", token, `{"playbook":"/srv/site.yml"}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("run status = %d, want 200 (%s)", code, body)
	}
	_, _, data := decodeEnvelope(t, body)
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil {
		t.Fatalf("data: %v", err)
	}
	return jv.JobID
}

// TestAPIPlaybookRunRefusesEscapingSource proves the HTTP seam refuses a
// path-escaping source: a --source playbook that resolves outside the checkout is an
// invalid_argument, refused before anything is enqueued, so a run recorded
// against a signed source can never execute a tree its signature never covered.
func TestAPIPlaybookRunRefusesEscapingSource(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	token := h.mint(t, "op", "operator")

	checkout := t.TempDir()
	if _, err := h.store.CreateSource(ctx, store.Source{
		Name: "signed-proj", Kind: store.SourcePath, URI: checkout, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}

	for _, playbook := range []string{"../../../../etc/attacker.yml", "/etc/attacker.yml"} {
		body := `{"source":"signed-proj","playbook":"` + playbook + `"}`
		code, _, resp := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token, body, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("playbook %q status = %d, want 400 (%s)", playbook, code, resp)
		}
		if kind, ecode, _ := decodeEnvelope(t, resp); kind != "error" || ecode != codeInvalidArgument {
			t.Fatalf("playbook %q envelope = %s, want error/invalid_argument", playbook, resp)
		}
	}
	jobs, err := h.store.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused --source run enqueued %d job(s)", len(jobs))
	}
}
