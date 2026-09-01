package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestRunbookLabel: the list and detail read a rule-triggered job's runbook
// without the "rule:" logic prefix; a non-rule runbook passes through.
func TestRunbookLabel(t *testing.T) {
	for _, tc := range []struct {
		runbook, want string
	}{
		{"rule:gate-ssh", "gate-ssh"},
		{"playbook", "playbook"},
	} {
		j := jobRow{Runbook: tc.runbook}
		if got := j.RunbookLabel(); got != tc.want {
			t.Errorf("RunbookLabel(%q) = %q, want %q", tc.runbook, got, tc.want)
		}
	}
}

// TestJobDetailRuleProvenance: a rule-triggered job's detail view links back to
// the rule its runbook named and to the event that raised it.
func TestJobDetailRuleProvenance(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "rule:gate-ssh", Kind: "playbook", Trigger: "rule",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	jobID := job.ID
	ev, err := h.store.CreateEvent(ctx, store.Event{
		Source: "check", MatchedRule: "gate-ssh", Outcome: store.OutcomeFired,
		JobID: &jobID, Normalized: `{"name":"at-home"}`,
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	if code != 200 {
		t.Fatalf("job detail status = %d", code)
	}
	for _, want := range []string{
		`fired by rule <a href="/rules">gate-ssh</a>`,
		`event <a href="/events/` + itoa(ev.ID) + `">#` + itoa(ev.ID) + `</a>`,
		`(<span class="mono">check</span> at-home) at`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("job detail missing provenance %q", want)
		}
	}
}

// TestJobDetailRuleNoEvent: a rule-runbook job with no event row renders the
// rule line and nothing more — no fabricated event link.
func TestJobDetailRuleNoEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "rule:gate-ssh", Kind: "playbook", Trigger: "rule",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	if code != 200 {
		t.Fatalf("job detail status = %d", code)
	}
	if !strings.Contains(page, `fired by rule <a href="/rules">gate-ssh</a>`) {
		t.Error("job detail missing the fired-by-rule line")
	}
	if strings.Contains(page, "/events/") {
		t.Error("a job without an event row must not render an event link")
	}
}

// TestJobDetailNoRule: a job whose runbook has no rule prefix shows no
// provenance line at all.
func TestJobDetailNoRule(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "manual-provision", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/jobs/"+itoa(job.ID))
	if code != 200 {
		t.Fatalf("job detail status = %d", code)
	}
	if strings.Contains(page, "fired by rule") {
		t.Error("a non-rule job must not render a fired-by-rule line")
	}
}
