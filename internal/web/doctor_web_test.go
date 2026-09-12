package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/store"
)

// findingSeed is one finding the test writes into a doctor job's event log,
// mirroring the daemon driver's per-entry finding event.
type findingSeed struct {
	name, status, detail string
}

// seedDoctorJob writes a kind=doctor job with finding events, the exact shape
// the daemon's doctor driver emits, so the job page and the identity strip
// read a real self-check result. The job is left queued — the harness's queue
// may claim and run it (the shell driver cannot execute the argv, which fails
// it harmlessly); the finding events are what this lane renders.
func seedDoctorJob(t *testing.T, h *harness, findings ...findingSeed) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: health.JobRunbook, Kind: health.JobKind, Trigger: "manual",
		Args: store.JobArgs{Argv: health.Argv()},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for _, f := range findings {
		raw := `{"name":"` + f.name + `","status":"` + f.status + `","detail":"` + f.detail + `"}`
		if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
			JobID: job.ID, Level: store.LevelSystem,
			Msg: f.name + " " + f.status + " — " + f.detail, RawJSON: raw,
		}); err != nil {
			t.Fatalf("AppendJobEvent: %v", err)
		}
	}
	return job
}

// TestDoctorVerbEnqueuesKindDoctor is the GATE for A1's invariant-5 enqueue:
// the Doctor verb queues the self-check as a kind=doctor job through the one
// queue — never runs it inline — and redirects to the job page.
func TestDoctorVerbEnqueuesKindDoctor(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/system/doctor", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("doctor verb status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/jobs/") {
		t.Fatalf("doctor verb redirect = %q, want /jobs/…", loc)
	}

	jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) == 0 {
		t.Fatal("doctor verb enqueued no job")
	}
	job := jobs[len(jobs)-1]
	if job.Kind != health.JobKind {
		t.Errorf("doctor job kind = %q, want %q", job.Kind, health.JobKind)
	}
	if job.Runbook != health.JobRunbook {
		t.Errorf("doctor job runbook = %q, want %q", job.Runbook, health.JobRunbook)
	}
}

// TestDoctorJobPageRendersFindingsRows is the GATE for A1's findings shape:
// a kind=doctor job's page renders each subsystem finding as a row (name ·
// status chip · detail) under a count header. Dropping the row render must
// red — the mutation gate.
func TestDoctorJobPageRendersFindingsRows(t *testing.T) {
	h := newHarness(t)
	job := seedDoctorJob(t, h,
		findingSeed{name: "data_dir", status: "ok", detail: "mode 700 — writable"},
		findingSeed{name: "secrets_protection", status: "warn", detail: "host key: absent; memory lock: locked"},
		findingSeed{name: "litestream", status: "unknown", detail: "binary not installed (optional); no database yet"})

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	if code != http.StatusOK {
		t.Fatalf("doctor job page = %d, want 200", code)
	}
	for _, want := range []string{
		`class="card mt14 doctor-findings"`,
		"Self-check · 3 findings",
		"data_dir",
		"mode 700 — writable",
		"secrets_protection",
		"host key: absent; memory lock: locked",
		"litestream",
		"binary not installed (optional); no database yet",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("doctor job page missing %q", want)
		}
	}
}

// TestSystemIdentityStripDoctorMeta is the GATE for A1's identity-strip meta:
// /system reads `doctor {age} ago · {n} findings` from the newest self-check
// job, linking to the job page, and the Doctor glyph posts the self-check.
func TestSystemIdentityStripDoctorMeta(t *testing.T) {
	h := newHarness(t)
	job := seedDoctorJob(t, h,
		findingSeed{name: "data_dir", status: "ok", detail: "mode 700 — writable"},
		findingSeed{name: "secrets_protection", status: "warn", detail: "host key: absent"})

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		`action="/system/doctor"`,
		" ago · 1 findings",
		`href="/jobs/` + itoa64(job.ID) + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("identity strip missing doctor meta %q", want)
		}
	}
}

// TestSystemIdentityStripDoctorMetaIdle: before any self-check has run, the
// strip renders the glyph alone — no fabricated age, no zero-count link.
func TestSystemIdentityStripDoctorMetaIdle(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	if !strings.Contains(page, `action="/system/doctor"`) {
		t.Error("identity strip missing the Doctor verb")
	}
	if strings.Contains(page, "findings") {
		t.Error("identity strip fabricates findings before a self-check ran")
	}
}
