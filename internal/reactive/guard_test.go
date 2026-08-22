package reactive_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// shortDedup is the window the guardrail tests that repeat one alert use. They
// step the clock past it deliberately, so what they assert is the guard under
// test rather than the dedup window swallowing everything.
const shortDedup = time.Second

func TestSilenceSwallowsByScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		silence   store.Silence
		alert     string
		host      string
		swallowed bool
		reason    string
	}{
		{
			name:    "alert scope matches its own alert",
			silence: store.Silence{ScopeKind: store.ScopeAlert, ScopeValue: "DiskFull"},
			alert:   "DiskFull", host: "web-01",
			swallowed: true, reason: "silenced: alert DiskFull until",
		},
		{
			name:    "alert scope leaves another alert alone",
			silence: store.Silence{ScopeKind: store.ScopeAlert, ScopeValue: "CertExpiring"},
			alert:   "DiskFull", host: "web-01",
		},
		{
			name:    "host scope matches the instance's host",
			silence: store.Silence{ScopeKind: store.ScopeHost, ScopeValue: "web-01"},
			alert:   "DiskFull", host: "web-01",
			swallowed: true, reason: "silenced: host web-01 until",
		},
		{
			name:    "host scope leaves another host alone",
			silence: store.Silence{ScopeKind: store.ScopeHost, ScopeValue: "web-02"},
			alert:   "DiskFull", host: "web-01",
		},
		{
			name:    "global scope is the kill switch",
			silence: store.Silence{ScopeKind: store.ScopeGlobal},
			alert:   "DiskFull", host: "web-01",
			swallowed: true, reason: "silenced: global pause until",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, harnessOptions{})
			h.addRule(t, shellRule("restart"))

			sil := tc.silence
			sil.ExpiresAt = time.Now().Add(2 * time.Hour)
			sil.CreatedVia = "cli"
			if _, err := h.store.CreateSilence(ctx, sil); err != nil {
				t.Fatalf("create silence: %v", err)
			}

			stored, err := h.engine.Ingest(ctx, h.firing(t, tc.alert, tc.host), []byte(`{}`))
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if !tc.swallowed {
				if stored.Outcome != store.OutcomeFired {
					t.Fatalf("outcome = %s (%s), want fired", stored.Outcome, stored.Reason)
				}
				return
			}
			if stored.Outcome != store.OutcomeSuppressed {
				t.Fatalf("outcome = %s, want suppressed", stored.Outcome)
			}
			if !strings.HasPrefix(stored.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to start with %q", stored.Reason, tc.reason)
			}
			if !strings.Contains(stored.Reason, "swallowed)") {
				t.Fatalf("reason %q carries no swallowed count", stored.Reason)
			}
		})
	}
}

// TestSilenceStopsApplyingWhenItExpires: there is no never-expires form, and
// nothing has to sweep for a silence to lift — expiry is decided at read time.
func TestSilenceStopsApplyingWhenItExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	h.addRule(t, shellRule("restart"))

	if _, err := h.store.CreateSilence(ctx, store.Silence{
		ScopeKind: store.ScopeGlobal,
		CreatedAt: time.Now().Add(-time.Hour),
		ExpiresAt: time.Now().Add(80 * time.Millisecond),
	}); err != nil {
		t.Fatalf("create silence: %v", err)
	}

	first, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest under the silence: %v", err)
	}
	if first.Outcome != store.OutcomeSuppressed {
		t.Fatalf("outcome under the silence = %s, want suppressed", first.Outcome)
	}

	time.Sleep(150 * time.Millisecond)

	// The swallowed event is not a dedup anchor, so the retry after the
	// silence lifts is the first one the window has ever seen.
	second, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest after the silence: %v", err)
	}
	if second.Outcome != store.OutcomeFired {
		t.Fatalf("outcome after the silence = %s (%s), want fired", second.Outcome, second.Reason)
	}
}

func TestDedupSwallowsARepeatInsideTheWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{dedupWindow: 5 * time.Minute})
	h.addRule(t, shellRule("restart"))

	first, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if first.Outcome != store.OutcomeFired {
		t.Fatalf("first outcome = %s (%s), want fired", first.Outcome, first.Reason)
	}

	h.advance(time.Minute)
	second, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if second.Outcome != store.OutcomeSuppressed {
		t.Fatalf("second outcome = %s, want suppressed", second.Outcome)
	}
	want := "duplicate of event " + strconv.FormatInt(first.ID, 10) + " within 5m"
	if second.Reason != want {
		t.Fatalf("reason = %q, want %q", second.Reason, want)
	}

	// A different host is a different condition.
	other, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-02"), []byte(`{}`))
	if err != nil {
		t.Fatalf("other-host ingest: %v", err)
	}
	if other.Outcome != store.OutcomeFired {
		t.Fatalf("other-host outcome = %s (%s), want fired", other.Outcome, other.Reason)
	}

	// Past the window the same condition is actionable again, and the anchor
	// is the fired event rather than the swallowed repeat.
	h.advance(6 * time.Minute)
	third, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("third ingest: %v", err)
	}
	if third.Outcome != store.OutcomeFired {
		t.Fatalf("third outcome = %s (%s), want fired", third.Outcome, third.Reason)
	}
}

// TestDedupAnchorIsNotPlantedByAnInertEvent is the R1 red-team repro: an
// attacker pre-plants a benign event that matches no rule but carries the
// dedup key a real alert will share, hoping to have the real alert swallowed as
// its "duplicate". The anchor must be an event that actually matched a rule, so
// the inert plant cannot mute the alert that follows.
func TestDedupAnchorIsNotPlantedByAnInertEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{dedupWindow: 5 * time.Minute})
	h.addRule(t, shellRule("restart")) // matches DiskFull firing, nothing else.

	const key = "alertmanager:fp:shared"

	// The plant: same source and dedup key as the coming alert, but a name no
	// rule matches. It is recorded, outcome "ignored".
	plant := reactive.Event{
		Source: reactive.SourceAlertmanager, Name: "Benign", Status: "firing",
		DedupKey: key, ReceivedAt: h.nowValue,
	}
	planted, err := h.engine.Ingest(ctx, plant, []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest plant: %v", err)
	}
	if planted.Outcome != store.OutcomeIgnored {
		t.Fatalf("plant outcome = %s (%s), want ignored", planted.Outcome, planted.Reason)
	}

	// The real alert arrives a moment later, inside the dedup window, sharing
	// the key. It must fire, not be swallowed as a duplicate of the plant.
	h.advance(time.Minute)
	alert := reactive.Event{
		Source: reactive.SourceAlertmanager, Name: "DiskFull", Status: "firing",
		DedupKey: key, ReceivedAt: h.nowValue,
	}
	fired, err := h.engine.Ingest(ctx, alert, []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest alert: %v", err)
	}
	if fired.Outcome != store.OutcomeFired {
		t.Fatalf("alert outcome = %s (%s), want fired — an inert plant muted a real alert",
			fired.Outcome, fired.Reason)
	}

	// Legitimate dedup is preserved: a genuine repeat of the firing alert,
	// still inside the window, is anchored on the event that fired and swallowed.
	h.advance(time.Minute)
	repeat, err := h.engine.Ingest(ctx, alert, []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest repeat: %v", err)
	}
	if repeat.Outcome != store.OutcomeSuppressed {
		t.Fatalf("repeat outcome = %s (%s), want suppressed", repeat.Outcome, repeat.Reason)
	}
	want := "duplicate of event " + strconv.FormatInt(fired.ID, 10) + " within 5m"
	if repeat.Reason != want {
		t.Fatalf("repeat reason = %q, want %q", repeat.Reason, want)
	}
}

// TestCooldownReasonNamesTheNextEligibleTime pins the reason string the events
// design specifies: "cooldown 6h, next eligible HH:MM".
func TestCooldownReasonNamesTheNextEligibleTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{dedupWindow: shortDedup})
	rule := shellRule("restart")
	rule.CooldownS = int((6 * time.Hour).Seconds())
	created := h.addRule(t, rule)

	fired, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if fired.Outcome != store.OutcomeFired {
		t.Fatalf("first outcome = %s (%s), want fired", fired.Outcome, fired.Reason)
	}
	ledger, err := h.store.LastRuleFire(ctx, created.ID)
	if err != nil {
		t.Fatalf("last rule fire: %v", err)
	}

	h.advance(time.Minute)
	held, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if held.Outcome != store.OutcomeSuppressed {
		t.Fatalf("second outcome = %s (%s), want suppressed", held.Outcome, held.Reason)
	}
	want := "cooldown 6h, next eligible " + ledger.Ts.Add(6*time.Hour).UTC().Format("15:04")
	if held.Reason != want {
		t.Fatalf("reason = %q, want %q", held.Reason, want)
	}

	h.advance(6 * time.Hour)
	again, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("third ingest: %v", err)
	}
	if again.Outcome != store.OutcomeFired {
		t.Fatalf("outcome past the cooldown = %s (%s), want fired", again.Outcome, again.Reason)
	}
}

// TestDailyCapDisablesTheRuleAndPages: a cap that throttled silently would
// leave a misfiring rule firing forever at the cap rate.
func TestDailyCapDisablesTheRuleAndPages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{dedupWindow: shortDedup})
	rule := shellRule("restart")
	rule.DailyCap = 2
	h.addRule(t, rule)

	for i := range 2 {
		h.advance(2 * shortDedup)
		stored, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
		if err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
		if stored.Outcome != store.OutcomeFired {
			t.Fatalf("ingest %d outcome = %s (%s), want fired", i, stored.Outcome, stored.Reason)
		}
	}

	h.advance(2 * shortDedup)
	capped, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("capped ingest: %v", err)
	}
	if capped.Outcome != store.OutcomeSuppressed {
		t.Fatalf("capped outcome = %s (%s), want suppressed", capped.Outcome, capped.Reason)
	}
	if capped.Reason != "daily cap 2 reached; rule restart disabled" {
		t.Fatalf("reason = %q", capped.Reason)
	}

	stored, err := h.store.GetRule(ctx, "restart")
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if stored.Enabled {
		t.Fatal("the rule is still enabled after hitting its cap")
	}

	var paged *notify.Notification
	for i := range h.sent.sent {
		if h.sent.sent[i].Priority == notify.PriorityMax {
			paged = &h.sent.sent[i]
		}
	}
	if paged == nil {
		t.Fatalf("no max-priority notification was sent; got %+v", h.sent.sent)
	}
	if !strings.Contains(paged.Body, "restart") || !strings.Contains(paged.Body, "disabled") {
		t.Fatalf("cap notification body = %q", paged.Body)
	}

	// A disabled rule is out of the evaluated set, so the next event is
	// ignored rather than suppressed again.
	h.advance(2 * shortDedup)
	next, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("post-cap ingest: %v", err)
	}
	if next.Outcome != store.OutcomeIgnored {
		t.Fatalf("post-cap outcome = %s, want ignored", next.Outcome)
	}
}

// TestGuardLedgersSurviveARestart: the cooldown and the cap are rows, not
// counters in memory, so a crash-loop restart does not hand a rule a fresh
// budget. The store is closed and reopened between the two halves.
func TestGuardLedgersSurviveARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := t.TempDir() + "/exocomp.db"

	before := newHarness(t, harnessOptions{dedupWindow: shortDedup, dbPath: dbPath})
	rule := shellRule("restart")
	rule.CooldownS = int((6 * time.Hour).Seconds())
	before.addRule(t, rule)

	fired, err := before.engine.Ingest(ctx, before.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest before the restart: %v", err)
	}
	if fired.Outcome != store.OutcomeFired {
		t.Fatalf("outcome before the restart = %s (%s), want fired", fired.Outcome, fired.Reason)
	}
	if err := before.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	after := newHarness(t, harnessOptions{dedupWindow: shortDedup, dbPath: dbPath})
	after.advance(2 * shortDedup)
	held, err := after.engine.Ingest(ctx, after.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest after the restart: %v", err)
	}
	if held.Outcome != store.OutcomeSuppressed {
		t.Fatalf("outcome after the restart = %s (%s), want suppressed by the cooldown ledger",
			held.Outcome, held.Reason)
	}
	if !strings.HasPrefix(held.Reason, "cooldown 6h, next eligible ") {
		t.Fatalf("reason after the restart = %q", held.Reason)
	}
}

// TestGuardOrderIsTheLedgerOrder: silence comes first, so an event nobody
// wants to hear about cannot spend a rule's budget or, worse, trip the cap
// that disables the rule. A maintenance window must not leave the operator
// with a rule they have to re-enable afterwards.
//
// Mutation check: move guardDailyCap or guardCooldown ahead of guardSilence in
// runGuards and this fails — the silenced event reports the cap instead, and
// disables the rule on its way out.
func TestGuardOrderIsTheLedgerOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{dedupWindow: shortDedup})
	rule := shellRule("restart")
	rule.CooldownS = 1
	rule.DailyCap = 1
	created := h.addRule(t, rule)

	// Spend the rule's whole budget legitimately.
	fired, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if fired.Outcome != store.OutcomeFired {
		t.Fatalf("first outcome = %s (%s), want fired", fired.Outcome, fired.Reason)
	}

	// Now silence everything, the way a rack reboot would.
	if _, err := h.store.CreateSilence(ctx, store.Silence{
		ScopeKind: store.ScopeGlobal, ExpiresAt: time.Now().Add(time.Hour),
		Reason: "rack maintenance",
	}); err != nil {
		t.Fatalf("create silence: %v", err)
	}
	h.advance(2 * shortDedup)

	swallowed, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest under the silence: %v", err)
	}
	if !strings.HasPrefix(swallowed.Reason, "silenced:") {
		t.Fatalf("reason = %q, want the silence to have decided it first", swallowed.Reason)
	}

	still, err := h.store.GetRule(ctx, "restart")
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if !still.Enabled {
		t.Fatal("a silenced event disabled the rule; a maintenance window must not cost the cap")
	}

	// And it did not spend the ledger either.
	fires, err := h.store.CountRuleFires(ctx, created.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("count fires: %v", err)
	}
	if fires != 1 {
		t.Fatalf("%d fires recorded, want only the one that actually ran", fires)
	}
}

func TestTraceRecordsEveryConditionInOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	rule := shellRule("restart")
	rule.CooldownS = 600
	rule.DailyCap = 5
	h.addRule(t, rule)

	stored, err := h.engine.Ingest(ctx, h.firing(t, "DiskFull", "web-01"), []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	explained, err := reactive.Explain(ctx, h.store, stored.ID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}

	var steps []string
	for _, c := range explained.Trace {
		steps = append(steps, c.Step)
	}
	want := []string{
		reactive.StepRule, reactive.StepSilence, reactive.StepDedup,
		reactive.StepCooldown, reactive.StepCap, reactive.StepApproval,
	}
	if len(steps) != len(want) {
		t.Fatalf("trace steps = %v, want %v", steps, want)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Fatalf("trace steps = %v, want %v", steps, want)
		}
	}
	for _, c := range explained.Trace {
		if !c.Passed {
			t.Fatalf("condition %s did not pass on a fired event: %s", c.Step, c.Reason)
		}
	}

	rendered := explained.Render()
	for _, fragment := range []string{
		"outcome: fired", "rule:    restart", "cooldown 10m", "daily cap 5",
	} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("rendered trace does not contain %q:\n%s", fragment, rendered)
		}
	}
}
