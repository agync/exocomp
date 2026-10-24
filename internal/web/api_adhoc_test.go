package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// An ad-hoc run over the API executes from the named source's checkout and
// inherits the source's default egress at queue admission, as a run started on
// the daemon's own host does.
func TestAPIAdhocRunUsesTheSourceCheckoutAndDefaults(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	ctx := context.Background()
	root := t.TempDir()
	src, err := h.store.CreateSource(ctx, store.Source{Name: "infra", Kind: store.SourcePath, URI: root, Enabled: true})
	if err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	if _, err := h.store.SetSourceEgressPorts(ctx, src.ID, []int{443}); err != nil {
		t.Fatalf("SetSourceEgressPorts: %v", err)
	}

	code, _, body := bearerPost(t, ts.URL, "/api/v1/adhoc/run", token,
		`{"runbook":"ansible","host":"web","module":"ping","source":"infra","run_secrets":"api","runner":"capped"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", code, body)
	}
	kind, _, data := decodeEnvelope(t, body)
	if kind != kindJobEnqueued {
		t.Fatalf("kind = %q, want %q", kind, kindJobEnqueued)
	}
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil || jv.JobID == 0 {
		t.Fatalf("no job_id: %v (%s)", err, body)
	}
	job, err := h.store.GetJob(ctx, jv.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Runbook != "ansible" || job.Args.Source != "infra" || job.Args.Dir != root {
		t.Errorf("job = %q source %q dir %q, want ansible from infra at %q",
			job.Runbook, job.Args.Source, job.Args.Dir, root)
	}
	if !slices.Contains(job.Args.EgressPorts, 443) {
		t.Errorf("egress = %v, want the source default 443", job.Args.EgressPorts)
	}
	if job.Args.RunSecretBinding != "api" || job.Args.RunnerName != "capped" {
		t.Errorf("run-secrets %q runner %q, want api and capped", job.Args.RunSecretBinding, job.Args.RunnerName)
	}
	if argv := strings.Join(job.Args.Argv, " "); !strings.Contains(argv, "--module-name ping") {
		t.Errorf("argv = %q, want the ping module", argv)
	}
}

func TestAPIAdhocRunRequireApprovalGates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/adhoc/run", token,
		`{"host":"web","args":"reboot","require_approval":true}`, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", code, body)
	}
	kind, _, data := decodeEnvelope(t, body)
	if kind != kindApprovalPending {
		t.Fatalf("kind = %q, want %q", kind, kindApprovalPending)
	}
	var p approvalPendingView
	if err := json.Unmarshal(data, &p); err != nil || p.ApprovalID == 0 || p.TTLSeconds <= 0 {
		t.Fatalf("pending = %+v (%v)", p, err)
	}
	job, err := h.store.GetJob(context.Background(), p.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.StateAwaitingApproval {
		t.Fatalf("job state = %s, want awaiting_approval", job.State)
	}
}

func TestAPIAdhocRunRefusals(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")
	ro := h.mint(t, "ro", "readonly")
	ap := h.mint(t, "ap", "approve")

	cases := []struct {
		name, token, body string
		status            int
	}{
		{"readonly token", ro, `{"host":"web","args":"uptime"}`, http.StatusForbidden},
		{"approve token", ap, `{"host":"web","args":"uptime"}`, http.StatusForbidden},
		{"no host", op, `{"args":"uptime"}`, http.StatusBadRequest},
		{"module with a flag", op, `{"host":"web","module":"-x"}`, http.StatusBadRequest},
		{"args with a flag", op, `{"host":"web","args":"--version"}`, http.StatusBadRequest},
		{"unknown runbook", op, `{"runbook":"shell","host":"web"}`, http.StatusBadRequest},
		{"identity not allowed", op, `{"host":"web","ssh_identity_files":["/srv/key"]}`, http.StatusBadRequest},
		{"unknown source", op, `{"host":"web","source":"nope"}`, http.StatusNotFound},
		// A relative inventory names nothing on the daemon; it is not resolved
		// against the daemon's working directory.
		{"relative inventory", op, `{"host":"web","inventory":"hosts.yml"}`, http.StatusBadRequest},
		// The route offers no privileged run, and a field it does not take is
		// refused rather than dropped.
		{"privileged field", op, `{"host":"web","args":"id","privileged":true}`, http.StatusBadRequest},
		{"misspelt field", op, `{"host":"web","run_secret":["s"]}`, http.StatusBadRequest},
		{"trailing JSON", op, `{"host":"web"}{"host":"db"}`, http.StatusBadRequest},
		{"oversize body", op, `{"host":"web","args":"` + strings.Repeat("a", 1<<16) + `"}`,
			http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, body := bearerPost(t, ts.URL, "/api/v1/adhoc/run", tc.token, tc.body, nil)
			if code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", code, tc.status, body)
			}
		})
	}
	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused request enqueued %d job(s)", len(jobs))
	}
}

// A retried request under one Idempotency-Key lands one job.
func TestAPIAdhocRunIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")

	var ids []int64
	for range 2 {
		code, _, body := bearerPost(t, ts.URL, "/api/v1/adhoc/run", op, `{"host":"web","args":"uptime"}`,
			map[string]string{"Idempotency-Key": "retry-1"})
		if code != http.StatusOK {
			t.Fatalf("status = %d (%s)", code, body)
		}
		_, _, data := decodeEnvelope(t, body)
		var jv jobEnqueuedView
		if err := json.Unmarshal(data, &jv); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, jv.JobID)
	}
	if ids[0] != ids[1] {
		t.Fatalf("a retry enqueued a second job: %v", ids)
	}
}
