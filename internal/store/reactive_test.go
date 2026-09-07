package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

func openReactive(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestMigrationAddsReactiveTables(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	version, err := st.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 5 {
		t.Fatalf("schema version %d, want at least 5", version)
	}

	// The rebuilt schedule table has to keep the kinds it already served and
	// admit the new one, or the already-served rows stop firing.
	for _, kind := range []string{store.ScheduleShell, store.ScheduleBackup, store.ScheduleTofuPlan} {
		args := store.ScheduleArgs{Argv: []string{"true"}, Target: "prod"}
		if _, err := st.CreateSchedule(ctx, store.NewSchedule{
			Name: "sched-" + kind, CronExpr: "@every 1h", Kind: kind, Args: args,
		}); err != nil {
			t.Fatalf("create %s schedule: %v", kind, err)
		}
	}
	if _, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name: "bogus", CronExpr: "@every 1h", Kind: "nonsense",
	}); err == nil {
		t.Fatal("create schedule of an unknown kind: want an error, got none")
	}
}

func TestScheduleRowsSurviveTheRebuild(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "exocomp.db")
	ctx := context.Background()

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	created, err := st.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly", CronExpr: "0 0 3 * * *", Kind: store.ScheduleBackup,
		Args: store.ScheduleArgs{Target: "nas"}, JitterS: 30,
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	got, err := reopened.GetSchedule(ctx, "nightly")
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if got.ID != created.ID || got.CronExpr != created.CronExpr ||
		got.Args.Target != "nas" || got.JitterS != 30 {
		t.Fatalf("schedule after rebuild = %+v, want %+v", got, created)
	}
}

func TestRuleCRUD(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateRule(ctx, store.NewRule{
		Name:       "disk-full",
		CELExpr:    `event.name == "DiskFull"`,
		ActionKind: store.ActionShell,
		Action:     store.RuleAction{Argv: []string{"/bin/true"}},
		CooldownS:  3600,
		DailyCap:   3,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if !created.Enabled {
		t.Fatal("a new rule is disabled, want enabled")
	}
	if len(created.Action.Argv) != 1 || created.Action.Argv[0] != "/bin/true" {
		t.Fatalf("action argv = %v", created.Action.Argv)
	}

	if _, err := st.CreateRule(ctx, store.NewRule{
		Name: "disk-full", CELExpr: "true", ActionKind: store.ActionNotify,
	}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate rule name: err = %v, want ErrDuplicate", err)
	}

	if err := st.SetRuleEnabled(ctx, "disk-full", false); err != nil {
		t.Fatalf("disable rule: %v", err)
	}
	enabled, err := st.ListRules(ctx, true)
	if err != nil {
		t.Fatalf("list enabled rules: %v", err)
	}
	if len(enabled) != 0 {
		t.Fatalf("listed %d enabled rules after disabling the only one", len(enabled))
	}
	all, err := st.ListRules(ctx, false)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("listed %d rules, want 1", len(all))
	}

	if err := st.DeleteRule(ctx, "disk-full"); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	if _, err := st.GetRule(ctx, "disk-full"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get deleted rule: err = %v, want ErrNotFound", err)
	}
}

func TestRuleFireLedger(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	rule, err := st.CreateRule(ctx, store.NewRule{
		Name: "r", CELExpr: "true", ActionKind: store.ActionNotify,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if _, err := st.LastRuleFire(ctx, rule.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("last fire of a rule that never fired: err = %v, want ErrNotFound", err)
	}

	for range 3 {
		if _, err := st.RecordRuleFire(ctx, rule.ID, nil, ""); err != nil {
			t.Fatalf("record fire: %v", err)
		}
	}
	n, err := st.CountRuleFires(ctx, rule.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("count fires: %v", err)
	}
	if n != 3 {
		t.Fatalf("counted %d fires, want 3", n)
	}
	future, err := st.CountRuleFires(ctx, rule.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("count fires since the future: %v", err)
	}
	if future != 0 {
		t.Fatalf("counted %d fires since the future, want 0", future)
	}

	last, err := st.LastRuleFire(ctx, rule.ID)
	if err != nil {
		t.Fatalf("last fire: %v", err)
	}
	if last.RuleID != rule.ID {
		t.Fatalf("last fire names rule %d, want %d", last.RuleID, rule.ID)
	}
}

// TestRuleFireDedupLedger pins the per-key half of the fire ledger: a fire
// records the dedup key it was raised for, LastRuleFireByKey finds the newest
// fire of one key, and LastRuleFire stays per-rule so a webhook source keeps
// its whole-rule budget.
func TestRuleFireDedupLedger(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	rule, err := st.CreateRule(ctx, store.NewRule{
		Name: "r", CELExpr: "true", ActionKind: store.ActionNotify,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if _, err := st.LastRuleFireByKey(ctx, rule.ID, "check:a:laptop"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("last fire by key of a rule that never fired that key: err = %v, want ErrNotFound", err)
	}

	laptop, err := st.RecordRuleFire(ctx, rule.ID, nil, "check:a:laptop")
	if err != nil {
		t.Fatalf("record laptop fire: %v", err)
	}
	if laptop.DedupKey != "check:a:laptop" {
		t.Fatalf("recorded fire dedup key = %q", laptop.DedupKey)
	}
	if _, err := st.RecordRuleFire(ctx, rule.ID, nil, "check:a:nas"); err != nil {
		t.Fatalf("record nas fire: %v", err)
	}
	if _, err := st.RecordRuleFire(ctx, rule.ID, nil, "check:a:laptop"); err != nil {
		t.Fatalf("record second laptop fire: %v", err)
	}

	perKey, err := st.LastRuleFireByKey(ctx, rule.ID, "check:a:laptop")
	if err != nil {
		t.Fatalf("last fire by key: %v", err)
	}
	if perKey.ID == laptop.ID {
		t.Fatal("LastRuleFireByKey did not return the newest fire of the key")
	}
	if perKey.DedupKey != "check:a:laptop" {
		t.Fatalf("by-key fire dedup key = %q", perKey.DedupKey)
	}

	perRule, err := st.LastRuleFire(ctx, rule.ID)
	if err != nil {
		t.Fatalf("last fire: %v", err)
	}
	if perRule.DedupKey != "check:a:laptop" {
		t.Fatalf("per-rule newest fire dedup key = %q, want the overall newest", perRule.DedupKey)
	}
}

func TestSilenceRefusesAnAbsentOrPastExpiry(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	if _, err := st.CreateSilence(ctx, store.Silence{
		ScopeKind: store.ScopeGlobal,
	}); err == nil {
		t.Fatal("silence with no expiry: want an error, got none")
	}
	if _, err := st.CreateSilence(ctx, store.Silence{
		ScopeKind: store.ScopeGlobal, ExpiresAt: time.Now().Add(-time.Minute),
	}); err == nil {
		t.Fatal("silence expiring in the past: want an error, got none")
	}
	if _, err := st.CreateSilence(ctx, store.Silence{
		ScopeKind: store.ScopeHost, ExpiresAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("host silence with no value: want an error, got none")
	}
	if _, err := st.CreateSilence(ctx, store.Silence{
		ScopeKind: "everything", ScopeValue: "x", ExpiresAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("silence of an unknown scope: want an error, got none")
	}
}

func TestSilenceExpiryIsARead(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	// A silence that expires between the write and the read stops applying
	// without anything sweeping for it.
	if _, err := st.CreateSilence(ctx, store.Silence{
		ScopeKind: store.ScopeAlert, ScopeValue: "DiskFull",
		CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(50 * time.Millisecond),
		CreatedVia: "cli",
	}); err != nil {
		t.Fatalf("create silence: %v", err)
	}

	active, err := st.ListSilences(ctx, true)
	if err != nil {
		t.Fatalf("list active silences: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("listed %d active silences, want 1", len(active))
	}

	time.Sleep(60 * time.Millisecond)

	active, err = st.ListSilences(ctx, true)
	if err != nil {
		t.Fatalf("list active silences: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("listed %d active silences after expiry, want 0", len(active))
	}
	all, err := st.ListSilences(ctx, false)
	if err != nil {
		t.Fatalf("list silences: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("listed %d silences in history, want 1", len(all))
	}
}

func TestEventOutcomesAreQueryable(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	for _, outcome := range []store.EventOutcome{
		store.OutcomeIgnored, store.OutcomeSuppressed, store.OutcomeGated, store.OutcomeFired,
	} {
		if _, err := st.CreateEvent(ctx, store.Event{
			Source:   "alertmanager",
			DedupKey: "k-" + string(outcome),
			Outcome:  outcome,
			Reason:   string(outcome) + " reason",
		}); err != nil {
			t.Fatalf("create %s event: %v", outcome, err)
		}
	}

	suppressed, err := st.ListEvents(ctx, store.EventFilter{
		Outcomes: []store.EventOutcome{store.OutcomeSuppressed},
	})
	if err != nil {
		t.Fatalf("list suppressed: %v", err)
	}
	if len(suppressed) != 1 || suppressed[0].Reason != "suppressed reason" {
		t.Fatalf("suppressed events = %+v", suppressed)
	}

	if _, err := st.CreateEvent(ctx, store.Event{
		Source: "generic", Outcome: "invented",
	}); err == nil {
		t.Fatal("event with an outcome outside the enum: want an error, got none")
	}
}

func TestCountEventsByOutcome(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	empty, err := st.CountEventsByOutcome(ctx)
	if err != nil {
		t.Fatalf("count events by outcome (empty store): %v", err)
	}
	if empty.Total() != 0 {
		t.Fatalf("empty store total = %d, want 0", empty.Total())
	}

	seed := map[store.EventOutcome]int{
		store.OutcomeIgnored:    2,
		store.OutcomeMatched:    1,
		store.OutcomeSuppressed: 3,
		store.OutcomeGated:      1,
		store.OutcomeFired:      4,
		store.OutcomeError:      1,
	}
	for outcome, n := range seed {
		for i := 0; i < n; i++ {
			if _, err := st.CreateEvent(ctx, store.Event{
				Source: "alertmanager", Outcome: outcome,
			}); err != nil {
				t.Fatalf("create %s event: %v", outcome, err)
			}
		}
	}

	got, err := st.CountEventsByOutcome(ctx)
	if err != nil {
		t.Fatalf("count events by outcome: %v", err)
	}
	want := store.EventOutcomeCounts{
		Ignored: 2, Matched: 1, Suppressed: 3, Gated: 1, Fired: 4, Error: 1,
	}
	if got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
	if got.Total() != 12 {
		t.Fatalf("total = %d, want 12", got.Total())
	}
}

// TestCountEventsByOutcomeSince is the GATE for the windowed funnel: events
// received at or after since are tallied by outcome, and an event older than
// the window is excluded. The web events page's last-24h funnel is built from
// it, so dropping the window (counting all-time) must flip this test red.
func TestCountEventsByOutcomeSince(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	since := time.Now().UTC().Add(-24 * time.Hour)

	seed := map[store.EventOutcome]int{
		store.OutcomeIgnored:    2,
		store.OutcomeMatched:    1,
		store.OutcomeSuppressed: 3,
		store.OutcomeGated:      1,
		store.OutcomeFired:      1,
		store.OutcomeError:      1,
	}
	for outcome, n := range seed {
		for i := 0; i < n; i++ {
			if _, err := st.CreateEvent(ctx, store.Event{
				Source: "alertmanager", Outcome: outcome,
				ReceivedAt: time.Now().Add(-time.Duration(i+1) * time.Hour),
			}); err != nil {
				t.Fatalf("create %s event: %v", outcome, err)
			}
		}
	}
	// An event older than the window must not count.
	if _, err := st.CreateEvent(ctx, store.Event{
		Source: "alertmanager", Outcome: store.OutcomeFired,
		ReceivedAt: time.Now().Add(-25 * time.Hour),
	}); err != nil {
		t.Fatalf("create stale event: %v", err)
	}

	got, err := st.CountEventsByOutcomeSince(ctx, since)
	if err != nil {
		t.Fatalf("count events by outcome since: %v", err)
	}
	want := store.EventOutcomeCounts{
		Ignored: 2, Matched: 1, Suppressed: 3, Gated: 1, Fired: 1, Error: 1,
	}
	if got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
	if got.Total() != 9 {
		t.Fatalf("total = %d, want 9", got.Total())
	}
}

// TestListEventsExcludesSource is the GATE for the EventFilter.NotSource
// clause the events page's "alerts" chip rides: rows from the excluded source
// never appear, rows from every other source do.
func TestListEventsExcludesSource(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()
	for _, src := range []string{"check", "alertmanager", "healthchecks"} {
		if _, err := st.CreateEvent(ctx, store.Event{
			Source: src, Outcome: store.OutcomeIgnored,
		}); err != nil {
			t.Fatalf("create %s event: %v", src, err)
		}
	}

	rows, err := st.ListEvents(ctx, store.EventFilter{NotSource: "check"})
	if err != nil {
		t.Fatalf("list events excluding source: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the non-check sources)", len(rows))
	}
	for _, e := range rows {
		if e.Source == "check" {
			t.Fatal("an excluded check row leaked through the filter")
		}
	}

	all, err := st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("list all events: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all rows = %d, want 3", len(all))
	}
}

func TestLastEventByDedupKeySkipsSuppressed(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	fired, err := st.CreateEvent(ctx, store.Event{
		Source: "alertmanager", DedupKey: "dk", Outcome: store.OutcomeFired,
		ReceivedAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("create fired event: %v", err)
	}
	if _, err := st.CreateEvent(ctx, store.Event{
		Source: "alertmanager", DedupKey: "dk", Outcome: store.OutcomeSuppressed,
	}); err != nil {
		t.Fatalf("create suppressed event: %v", err)
	}

	// A dedup window anchored on the suppressed repeat would slide forward
	// forever and never let the alert through again.
	got, err := st.LastEventByDedupKey(ctx, "dk")
	if err != nil {
		t.Fatalf("last event by dedup key: %v", err)
	}
	if got.ID != fired.ID {
		t.Fatalf("dedup anchor is event %d, want the fired event %d", got.ID, fired.ID)
	}
}

func TestApprovalDecisionIsSingleUse(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	job, err := st.CreateGatedJob(ctx, store.NewJob{
		Runbook: "rule:restart", Kind: "shell", Trigger: "event",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("create gated job: %v", err)
	}
	if job.State != store.StateAwaitingApproval {
		t.Fatalf("gated job is %s, want %s", job.State, store.StateAwaitingApproval)
	}

	approval, err := st.CreateApproval(ctx, store.Approval{
		JobID: job.ID, ExpiresAt: time.Now().Add(time.Hour), TokenHash: "hash",
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}

	if _, err := st.DecideApproval(ctx, approval.ID, store.ApprovalApproved, "ntfy", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	settled, err := st.DecideApproval(ctx, approval.ID, store.ApprovalRejected, "ntfy", "")
	if !errors.Is(err, store.ErrApprovalDecided) {
		t.Fatalf("second decision: err = %v, want ErrApprovalDecided", err)
	}
	if settled.State != store.ApprovalApproved {
		t.Fatalf("approval is %s after a refused second decision, want approved", settled.State)
	}
}

func TestPendingApprovalsExpireByClock(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	job, err := st.CreateGatedJob(ctx, store.NewJob{
		Runbook: "rule:x", Kind: "shell", Trigger: "event",
		Args: store.JobArgs{Argv: []string{"/bin/true"}}, DedupKey: "alert:DiskFull:web",
	})
	if err != nil {
		t.Fatalf("create gated job: %v", err)
	}
	approval, err := st.CreateApproval(ctx, store.Approval{
		JobID: job.ID, ExpiresAt: time.Now().Add(30 * time.Minute), TokenHash: "hash",
	})
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}

	due, err := st.PendingApprovalsExpiredAt(ctx, time.Now())
	if err != nil {
		t.Fatalf("expired approvals: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("%d approvals expired before their TTL", len(due))
	}

	due, err = st.PendingApprovalsExpiredAt(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("expired approvals: %v", err)
	}
	if len(due) != 1 || due[0].ID != approval.ID {
		t.Fatalf("expired approvals = %+v, want approval %d", due, approval.ID)
	}

	matched, err := st.PendingApprovalsByDedupKey(ctx, "alert:DiskFull:web")
	if err != nil {
		t.Fatalf("pending approvals by dedup key: %v", err)
	}
	if len(matched) != 1 || matched[0].ID != approval.ID {
		t.Fatalf("pending approvals by dedup key = %+v, want approval %d", matched, approval.ID)
	}
	other, err := st.PendingApprovalsByDedupKey(ctx, "alert:Other:web")
	if err != nil {
		t.Fatalf("pending approvals by another dedup key: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("dedup key of another alert matched %d approvals", len(other))
	}
}
