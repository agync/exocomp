package reactive_test

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// sortedHosts keeps a fixture's drift rows in a fixed order, so an assertion on
// the page body does not depend on map iteration.
func sortedHosts(drift map[string]int) []string {
	return slices.Sorted(maps.Keys(drift))
}

// checkRawJSON renders the outcome exactly as the ansible driver attaches it,
// so the fixture and the producer cannot disagree about the envelope key.
func checkRawJSON(t *testing.T, o executor.AnsibleCheckOutcome) string {
	t.Helper()
	raw, err := json.Marshal(map[string]executor.AnsibleCheckOutcome{"ansible_check": o})
	if err != nil {
		t.Fatalf("encode check outcome: %v", err)
	}
	return string(raw)
}

// checkJob writes the row a finished scheduled `--check` play leaves behind,
// including the check outcome the ansible driver attaches once it has parsed
// the PLAY RECAP. drift maps a host to how many tasks would change; an empty
// map is a clean check.
func (h *harness) checkJob(t *testing.T, runbook, playbook string, drift map[string]int) store.Job {
	t.Helper()
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: runbook,
		Kind:    executor.KindAnsible,
		Trigger: "schedule",
		Args:    store.JobArgs{Argv: executor.AnsibleArtifactArgv(playbook, "--check")},
	})
	if err != nil {
		t.Fatalf("create check job: %v", err)
	}

	outcome := executor.AnsibleCheckOutcome{Operation: "check"}
	for _, host := range sortedHosts(drift) {
		outcome.Changes = true
		outcome.Drift = append(outcome.Drift,
			executor.AnsibleHostDrift{Host: host, Changed: drift[host]})
	}
	if len(outcome.Drift) == 0 {
		outcome.CleanHosts = 2
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: job.ID, Level: store.LevelSystem, Msg: "ansible check finished",
		RawJSON: checkRawJSON(t, outcome),
	}); err != nil {
		t.Fatalf("append check outcome: %v", err)
	}

	if err := h.store.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("start check job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("finish check job: %v", err)
	}
	return job
}

// TestAnsibleCheckDriftPagesOnceWithStableIdentity is the gap this lane closes:
// the ansible half of a homelab was invisible to the drift sweep, so a check
// play that found changes paged nobody.
func TestAnsibleCheckDriftPagesOnceWithStableIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.checkJob(t, "nightly-converge", "playbooks/site.yml", map[string]int{"nas": 3, "media": 1})

	raised, err := h.engine.SweepDrift(ctx)
	if err != nil {
		t.Fatalf("sweep drift: %v", err)
	}
	if raised != 1 {
		t.Fatalf("raised %d drift events, want 1", raised)
	}

	pages := driftPages(h.sent.sent)
	if len(pages) != 1 {
		t.Fatalf("ansible drift raised %d pages, want 1", len(pages))
	}
	open := pages[0]
	if open.Resolved {
		t.Fatal("opening ansible drift page is Resolved; it must open the incident")
	}
	if open.Host != "nightly-converge/site.yml" {
		t.Fatalf("opening identity host = %q, want the play subject", open.Host)
	}

	event := lastEvent(t, h.store)
	if event.Source != "drift" {
		t.Fatalf("event source = %q, want drift", event.Source)
	}
	if !strings.Contains(event.Normalized, "AnsibleDrift") {
		t.Fatalf("event is not an AnsibleDrift: %s", event.Normalized)
	}
	if !strings.Contains(event.Raw, `"nas"`) || !strings.Contains(event.Raw, `"media"`) {
		t.Fatalf("event payload does not name the drifted hosts: %s", event.Raw)
	}

	// A second sweep pages nothing: the event table is the checkpoint.
	again, err := h.engine.SweepDrift(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("second sweep raised %d events", again)
	}
	if len(driftPages(h.sent.sent)) != 1 {
		t.Fatalf("second sweep paged again: %d pages", len(driftPages(h.sent.sent)))
	}
}

// TestACheckWithNoChangesRaisesNothing is the ansible twin of
// TestAPlanWithNoChangesRaisesNothing: a dry run that would change nothing is
// not drift.
func TestACheckWithNoChangesRaisesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.checkJob(t, "nightly-converge", "playbooks/site.yml", nil)

	raised, err := h.engine.SweepDrift(ctx)
	if err != nil {
		t.Fatalf("sweep drift: %v", err)
	}
	if raised != 0 {
		t.Fatalf("a clean check raised %d events", raised)
	}
}

// TestAnsibleDriftGatesARule pins that an ansible drift reaches the same CEL
// gate a tofu drift does, which is the point of routing it through Ingest.
func TestAnsibleDriftGatesARule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.addRule(t, store.NewRule{
		Name:       "shout-on-play-drift",
		CELExpr:    `event.name == "AnsibleDrift" && event.labels["runbook"] == "nightly-converge"`,
		ActionKind: store.ActionNotify,
	})
	h.checkJob(t, "nightly-converge", "playbooks/site.yml", map[string]int{"nas": 1})

	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift: %v", err)
	}
	event := lastEvent(t, h.store)
	if event.MatchedRule != "shout-on-play-drift" {
		t.Fatalf("matched rule = %q, want the ansible drift rule", event.MatchedRule)
	}
	if event.Outcome != store.OutcomeMatched {
		t.Fatalf("outcome = %q", event.Outcome)
	}
}
