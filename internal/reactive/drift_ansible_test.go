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

// TestAnsibleCheckDriftPagesOnceWithStableIdentity closes a gap where
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

// TestAnsibleCheckDriftAutoCloses pins the other half: a later clean check for
// the same subject closes the exact incident the drift opened, once.
func TestAnsibleCheckDriftAutoCloses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.checkJob(t, "nightly-converge", "playbooks/site.yml", map[string]int{"nas": 3})
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (open): %v", err)
	}
	opens := driftPages(h.sent.sent)
	if len(opens) != 1 {
		t.Fatalf("open pages = %d, want 1", len(opens))
	}

	h.checkJob(t, "nightly-converge", "playbooks/site.yml", nil)
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (close): %v", err)
	}

	pages := driftPages(h.sent.sent)
	if len(pages) != 2 {
		t.Fatalf("after the clean check there are %d pages, want 2 (open, close)", len(pages))
	}
	closing := pages[1]
	if !closing.Resolved {
		t.Fatalf("close page is not Resolved: %q", closing.Title)
	}
	if closing.Source != opens[0].Source || closing.Host != opens[0].Host {
		t.Fatalf("close identity = %q/%q, want the opening %q/%q",
			closing.Source, closing.Host, opens[0].Source, opens[0].Host)
	}

	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (idempotent): %v", err)
	}
	if again := driftPages(h.sent.sent); len(again) != 2 {
		t.Fatalf("third sweep produced %d pages, want the same 2", len(again))
	}
}

// TestCleanAnsibleCheckWithoutPriorDriftClosesNothing pins that a play that
// never drifted raises no close.
func TestCleanAnsibleCheckWithoutPriorDriftClosesNothing(t *testing.T) {
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
	if pages := driftPages(h.sent.sent); len(pages) != 0 {
		t.Fatalf("a clean check with no prior drift produced %d pages, want 0", len(pages))
	}
}

// TestAnsibleDriftSubjectsDoNotCrossClose pins the reason the playbook is part
// of the subject: every manual `playbook run` shares the runbook "playbook", so
// a subject of runbook alone would let one play's clean check close another
// play's incident.
func TestAnsibleDriftSubjectsDoNotCrossClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.checkJob(t, "playbook", "/srv/a/site.yml", map[string]int{"nas": 1})
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (open): %v", err)
	}
	h.checkJob(t, "playbook", "/srv/b/other.yml", nil)
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (other play clean): %v", err)
	}

	pages := driftPages(h.sent.sent)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want only the one open page", len(pages))
	}
	if pages[0].Resolved {
		t.Fatal("a clean check of a different play closed this play's incident")
	}
}

// TestAnsibleRunWithoutCheckOutcomeIsNotDrift pins that an ordinary converge —
// which carries no check payload — neither opens nor closes anything. A run
// that was never a dry run is no evidence about drift.
func TestAnsibleRunWithoutCheckOutcomeIsNotDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.checkJob(t, "nightly-converge", "playbooks/site.yml", map[string]int{"nas": 2})
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (open): %v", err)
	}

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "nightly-converge",
		Kind:    executor.KindAnsible,
		Trigger: "schedule",
		Args:    store.JobArgs{Argv: executor.AnsibleArtifactArgv("playbooks/site.yml")},
	})
	if err != nil {
		t.Fatalf("create converge job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("start converge job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("finish converge job: %v", err)
	}

	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (converge): %v", err)
	}
	pages := driftPages(h.sent.sent)
	if len(pages) != 1 || pages[0].Resolved {
		t.Fatalf("a plain converge changed the incident: %d pages", len(pages))
	}
}

// TestDegradedCleanCheckDoesNotClose pins that a check which could not reach
// every host is not evidence of a clean play: it saw nothing at all on the
// hosts it never reached, so the incident stays open.
func TestDegradedCleanCheckDoesNotClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.checkJob(t, "nightly-converge", "playbooks/site.yml", map[string]int{"nas": 2})
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (open): %v", err)
	}

	degraded := h.checkJob(t, "nightly-converge", "playbooks/site.yml", nil)
	if err := h.store.SetJobPartial(ctx, degraded.ID, true); err != nil {
		t.Fatalf("mark degraded: %v", err)
	}
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (degraded): %v", err)
	}

	pages := driftPages(h.sent.sent)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want only the open page", len(pages))
	}
	if pages[0].Resolved {
		t.Fatal("a degraded check closed the incident")
	}
}

// degradedCheckJob writes a finished scheduled `--check` play that settled
// degraded — the state-truth form of the tolerated run
// TestDegradedCleanCheckDoesNotClose exercises through the legacy partial flag.
func (h *harness) degradedCheckJob(t *testing.T, runbook, playbook string, drift map[string]int) store.Job {
	t.Helper()
	ctx := context.Background()

	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: runbook,
		Kind:    executor.KindAnsible,
		Trigger: "schedule",
		Args:    store.JobArgs{Argv: executor.AnsibleArtifactArgv(playbook, "--check")},
	})
	if err != nil {
		t.Fatalf("create degraded check job: %v", err)
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
		t.Fatalf("start degraded check job: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateDegraded); err != nil {
		t.Fatalf("settle degraded check job: %v", err)
	}
	return job
}

// TestDegradedStateCheckStillOpensDrift pins B3: a scheduled --check run that
// settles degraded still opens the drift incident. Before the sweep admitted
// the degraded state, such a run was invisible to it and the drift went
// unpaged — the tolerated run's findings are real config drift either way.
func TestDegradedStateCheckStillOpensDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.degradedCheckJob(t, "nightly-converge", "playbooks/site.yml", map[string]int{"nas": 3})

	raised, err := h.engine.SweepDrift(ctx)
	if err != nil {
		t.Fatalf("sweep drift (degraded): %v", err)
	}
	if raised != 1 {
		t.Fatalf("raised %d drift events for a degraded check, want 1", raised)
	}
	if len(driftPages(h.sent.sent)) != 1 {
		t.Fatalf("degraded check produced %d pages, want the one open", len(driftPages(h.sent.sent)))
	}
}

// TestDegradedStateCleanCheckDoesNotClose pins that a clean check which settled
// degraded is not evidence of a clean play: it saw nothing on the hosts it
// never reached, so the incident stays open — the state truth, not just the
// legacy partial flag, keeps the incident.
func TestDegradedStateCleanCheckDoesNotClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.checkJob(t, "nightly-converge", "playbooks/site.yml", map[string]int{"nas": 2})
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (open): %v", err)
	}

	h.degradedCheckJob(t, "nightly-converge", "playbooks/site.yml", nil)
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (degraded clean): %v", err)
	}

	pages := driftPages(h.sent.sent)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want only the open page", len(pages))
	}
	if pages[0].Resolved {
		t.Fatal("a degraded clean check closed the incident")
	}
}

// TestFailedCheckDoesNotClose pins that only a check that actually finished
// clean closes an incident: a check that failed decided nothing about drift,
// whatever partial recap it printed on the way down.
func TestFailedCheckDoesNotClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.checkJob(t, "nightly-converge", "playbooks/site.yml", map[string]int{"nas": 2})
	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (open): %v", err)
	}

	failed, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "nightly-converge",
		Kind:    executor.KindAnsible,
		Trigger: "schedule",
		Args:    store.JobArgs{Argv: executor.AnsibleArtifactArgv("playbooks/site.yml", "--check")},
	})
	if err != nil {
		t.Fatalf("create failed check: %v", err)
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: failed.ID, Level: store.LevelSystem, Msg: "ansible check finished",
		RawJSON: checkRawJSON(t, executor.AnsibleCheckOutcome{Operation: "check", CleanHosts: 1}),
	}); err != nil {
		t.Fatalf("append check outcome: %v", err)
	}
	if err := h.store.TransitionJob(ctx, failed.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("start failed check: %v", err)
	}
	if err := h.store.TransitionJob(ctx, failed.ID, store.StateRunning, store.StateFailed); err != nil {
		t.Fatalf("fail check: %v", err)
	}

	if _, err := h.engine.SweepDrift(ctx); err != nil {
		t.Fatalf("sweep drift (failed check): %v", err)
	}
	pages := driftPages(h.sent.sent)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want only the open page", len(pages))
	}
	if pages[0].Resolved {
		t.Fatal("a failed check closed the incident")
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
