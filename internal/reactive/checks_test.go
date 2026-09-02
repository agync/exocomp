package reactive_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// tcpTarget opens a real loopback listener and returns its address and port,
// so a sweep's tcp probe answers real dial truth. shut closes the listener,
// which is what "the host left the LAN" means to the probe.
func tcpTarget(t *testing.T) (addr string, port int, shut func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatalf("split listener addr: %v", err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("listener port: %v", err)
	}
	return host, p, func() { _ = ln.Close() }
}

// reopenTCP binds a listener on the exact address a check targets, which is
// how a test brings a host back without a target-edit verb (v1 has none).
func reopenTCP(t *testing.T, addr string, port int) func() {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("reopen listener on %s:%d: %v", addr, port, err)
	}
	return func() { _ = ln.Close() }
}

// waitUntilRefused polls a target until a dial is refused, so the kernel's
// teardown of a just-closed listener has settled before a sweep asserts down.
// A probe landing in the close-to-teardown window would otherwise still connect
// and read as a stale up — a real kernel artifact the hysteresis absorbs but an
// exact-count assertion cannot.
func waitUntilRefused(t *testing.T, addr string, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	d := net.Dialer{Timeout: 50 * time.Millisecond}
	for time.Now().Before(deadline) {
		conn, err := d.Dial("tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("port %s:%d still accepts after the listener closed", addr, port)
}

// addTCPCheck registers one single-target tcp check with a 30s interval.
func (h *harness) addTCPCheck(t *testing.T, name, addr string, port int) store.Check {
	t.Helper()
	c, err := h.store.CreateCheck(context.Background(), store.NewCheck{
		Name: name, Kind: store.CheckTCP, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "laptop", Address: addr, Port: port}},
	})
	if err != nil {
		t.Fatalf("create check %q: %v", name, err)
	}
	return c
}

// sweepN runs n sweep passes, advancing the harness clock one interval between
// them, and returns the events raised by the final pass.
func (h *harness) sweepN(t *testing.T, n int) int {
	t.Helper()
	var raised int
	for i := 0; i < n; i++ {
		r, err := h.engine.SweepChecks(context.Background())
		if err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
		raised = r
		h.advance(30 * time.Second)
	}
	return raised
}

// TestCheckSweepHysteresisAndColdStart is the presence loop end to end: a
// target home at daemon start is seeded without emitting, a steady state never
// emits however far past hysteresis it runs, and a down then up transition each
// produce exactly one event with the netgroup label and the per-target dedup
// key.
func TestCheckSweepHysteresisAndColdStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	addr, port, shut := tcpTarget(t)
	defer shut()
	c := h.addTCPCheck(t, "at-home", addr, port)

	// Cold-start seed: the first sweep probes and records without emitting, so
	// a daemon restart never reads an up target as a fresh transition.
	raised, err := h.engine.SweepChecks(ctx)
	if err != nil {
		t.Fatalf("seed sweep: %v", err)
	}
	if raised != 0 {
		t.Fatalf("seed sweep raised %d events, want 0", raised)
	}
	state, err := h.store.GetCheckState(ctx, c.ID, "laptop")
	if err != nil {
		t.Fatalf("get state after seed: %v", err)
	}
	if state.Status != store.CheckUp || state.Emitted != store.CheckUp || state.Consecutive != 1 {
		t.Fatalf("seeded state = %+v, want status=up emitted=up consecutive=1", state)
	}
	// The cold-start probe opened the observation ring with the seeded result.
	if len(state.RecentObservations) == 0 {
		t.Fatal("seeded state has no observation ring")
	}

	// Steady up never emits, even past hysteresis: emitted is seeded to the
	// result, so `emitted != status` stays false however long the target stays
	// home. Mutation check: seed emitted as '' and the 3rd sweep below emits,
	// failing this test — a restart would spurious-fire every up target.
	raised = h.sweepN(t, reactive.DefaultHysteresis)
	if raised != 0 {
		t.Fatalf("steady up raised %d events; a target that never left must not emit", raised)
	}

	// A netgroup covering the target means the resolved event names it instead
	// of "away".
	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name:     "loop",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	}); err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	// The host leaves: only after N consecutive down observations is the
	// departure declared.
	shut()
	waitUntilRefused(t, addr, port)
	h.advance(30 * time.Second)
	raised = h.sweepN(t, reactive.DefaultHysteresis)
	if raised != 1 {
		t.Fatalf("departure raised %d events, want 1", raised)
	}
	resolved := lastEvent(t, h.store)
	if resolved.Outcome != store.OutcomeIgnored {
		t.Fatalf("resolved outcome = %s (%s), want ignored (no rule)", resolved.Outcome, resolved.Reason)
	}
	if resolved.DedupKey != "check:at-home:laptop" {
		t.Fatalf("resolved dedup key = %q, want check:at-home:laptop", resolved.DedupKey)
	}
	var normalized reactive.Event
	if err := json.Unmarshal([]byte(resolved.Normalized), &normalized); err != nil {
		t.Fatalf("decode normalized resolved event: %v", err)
	}
	if normalized.Status != reactive.StatusResolved {
		t.Fatalf("resolved event status = %q", normalized.Status)
	}
	if normalized.Labels["netgroup"] != "loop" || normalized.Labels["target"] != "laptop" {
		t.Fatalf("resolved labels = %+v, want netgroup=loop target=laptop", normalized.Labels)
	}
	// The resolved event carries the hysteresis ledger that produced it: the
	// last 3 down observations, homogeneous (never mixed with the earlier up
	// seed), ending at 3-of-3. This is the "WHY THIS COUNTED" card's data.
	assertLedger(t, resolved.CheckObservations, store.CheckDown)

	// The host returns: the same hysteresis before the join is declared.
	reopen := reopenTCP(t, addr, port)
	defer reopen()
	h.advance(30 * time.Second)
	raised = h.sweepN(t, reactive.DefaultHysteresis)
	if raised != 1 {
		t.Fatalf("join raised %d events, want 1", raised)
	}
	joined := lastEvent(t, h.store)
	if err := json.Unmarshal([]byte(joined.Normalized), &normalized); err != nil {
		t.Fatalf("decode normalized join event: %v", err)
	}
	if normalized.Status != reactive.StatusFiring || normalized.Labels["netgroup"] != "loop" {
		t.Fatalf("join event = %+v, want firing with netgroup=loop", normalized)
	}
}

// TestCheckRuleFiresAJob is the loop the check exists for: a declared join is
// an event in the same pipeline as any webhook, so a rule turns it into a job
// on the one queue.
func TestCheckRuleFiresAJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	addr, port, shut := tcpTarget(t)
	defer shut()
	h.addTCPCheck(t, "at-home", addr, port)
	h.addRule(t, store.NewRule{
		Name:         "wake",
		SourceFilter: reactive.SourceCheck,
		CELExpr:      `event.name == "at-home" && event.status == "firing"`,
		ActionKind:   store.ActionShell,
		Action:       store.RuleAction{Argv: []string{"/bin/true"}},
	})

	if _, err := h.engine.SweepChecks(ctx); err != nil {
		t.Fatalf("seed sweep: %v", err)
	}

	shut()
	waitUntilRefused(t, addr, port)
	h.advance(30 * time.Second)
	h.sweepN(t, reactive.DefaultHysteresis)
	resolved := lastEvent(t, h.store)
	if resolved.Outcome != store.OutcomeIgnored {
		t.Fatalf("resolved outcome = %s (%s), want ignored — the rule fires only on a join",
			resolved.Outcome, resolved.Reason)
	}

	reopen := reopenTCP(t, addr, port)
	defer reopen()
	h.advance(30 * time.Second)
	h.sweepN(t, reactive.DefaultHysteresis)
	fired := lastEvent(t, h.store)
	if fired.Outcome != store.OutcomeFired {
		t.Fatalf("join outcome = %s (%s), want fired", fired.Outcome, fired.Reason)
	}
	if fired.MatchedRule != "wake" {
		t.Fatalf("matched rule = %q, want wake", fired.MatchedRule)
	}
	if fired.JobID == nil {
		t.Fatal("the join event carried no job")
	}
}

// TestCheckCooldownIsPerTarget pins the check-source carve-out: each target of
// a multi-target check has its own cooldown slot, while a webhook matching the
// same rule still shares the rule's whole budget.
//
// Mutation check: revert guardCooldown to the per-rule lookup for check
// sources and the nas fire below is suppressed by the laptop fire a second
// earlier — this test's "both fired" assertion fails.
func TestCheckCooldownIsPerTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	addrA, portA, closeA := tcpTarget(t)
	addrB, portB, closeB := tcpTarget(t)
	// Both listeners closed, so the seed observes both targets away.
	closeA()
	waitUntilRefused(t, addrA, portA)
	closeB()
	waitUntilRefused(t, addrB, portB)

	check, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "home", Kind: store.CheckTCP, IntervalS: 30,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: addrA, Port: portA},
			{Name: "nas", Address: addrB, Port: portB},
		},
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	// Source filter empty on purpose: the same rule must admit a webhook event
	// so the per-rule side of the split is observable.
	h.addRule(t, store.NewRule{
		Name:       "join",
		CELExpr:    `event.name == "home" && event.status == "firing"`,
		ActionKind: store.ActionShell,
		Action:     store.RuleAction{Argv: []string{"/bin/true"}},
		CooldownS:  int(time.Hour.Seconds()),
	})

	if _, err := h.engine.SweepChecks(ctx); err != nil {
		t.Fatalf("seed sweep: %v", err)
	}

	reopenA := reopenTCP(t, addrA, portA)
	defer reopenA()
	reopenB := reopenTCP(t, addrB, portB)
	defer reopenB()
	h.advance(30 * time.Second)
	h.sweepN(t, reactive.DefaultHysteresis)

	events, err := h.store.ListEvents(ctx, store.EventFilter{Source: reactive.SourceCheck})
	if err != nil {
		t.Fatalf("list check events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d check events, want 2 (both targets joined)", len(events))
	}
	for _, ev := range events {
		if ev.Outcome != store.OutcomeFired {
			t.Fatalf("event %s outcome = %s (%s), want fired — a per-rule cooldown suppressed a per-target join",
				ev.DedupKey, ev.Outcome, ev.Reason)
		}
	}
	_ = check

	// A webhook matching the same rule inside the cooldown is suppressed
	// per-rule: it is not a check key, so LastRuleFire — not the per-key
	// lookup — decides.
	webhook := h.firing(t, "home", "web-01")
	h.advance(time.Minute)
	webhook.ReceivedAt = h.nowValue
	held, err := h.engine.Ingest(ctx, webhook, []byte(`{}`))
	if err != nil {
		t.Fatalf("ingest webhook: %v", err)
	}
	if held.Outcome != store.OutcomeSuppressed || !strings.HasPrefix(held.Reason, "cooldown 1h") {
		t.Fatalf("webhook outcome = %s (%s), want suppressed by the per-rule cooldown",
			held.Outcome, held.Reason)
	}
}

// TestCheckResolveWithdrawsThenFollowOnIsSuppressed pins the interplay of the
// dedup window and the resolve path when a check flaps inside 5 minutes: the
// resolve still withdraws the pending approval (the short-circuit happens
// before the guards), and the next join is then a duplicate of the resolved
// anchor and suppressed — the pipeline stays churn-free on a fast flap.
func TestCheckResolveWithdrawsThenFollowOnIsSuppressed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	addr, port, shut := tcpTarget(t)
	shut() // seed down
	waitUntilRefused(t, addr, port)
	h.addTCPCheck(t, "at-home", addr, port)
	h.addRule(t, store.NewRule{
		Name:            "wake",
		SourceFilter:    reactive.SourceCheck,
		CELExpr:         `event.name == "at-home" && event.status == "firing"`,
		ActionKind:      store.ActionShell,
		Action:          store.RuleAction{Argv: []string{"/bin/true"}},
		RequireApproval: true,
	})

	if _, err := h.engine.SweepChecks(ctx); err != nil {
		t.Fatalf("seed sweep: %v", err)
	}

	// Join: the rule requires approval, so the firing gates instead of firing.
	reopen := reopenTCP(t, addr, port)
	h.advance(30 * time.Second)
	h.sweepN(t, reactive.DefaultHysteresis)
	gated := lastEvent(t, h.store)
	if gated.Outcome != store.OutcomeGated {
		t.Fatalf("join outcome = %s (%s), want gated", gated.Outcome, gated.Reason)
	}
	approval, err := h.store.ApprovalByJob(ctx, *gated.JobID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if approval.State != store.ApprovalPending {
		t.Fatalf("approval state = %s, want pending", approval.State)
	}

	// Departure inside the window: the resolve withdraws the pending approval
	// before any guardrail can see it.
	reopen()
	waitUntilRefused(t, addr, port)
	h.advance(30 * time.Second)
	h.sweepN(t, reactive.DefaultHysteresis)
	resolved := lastEvent(t, h.store)
	if resolved.Outcome != store.OutcomeMatched || !strings.Contains(resolved.Reason, "withdrew 1") {
		t.Fatalf("resolve outcome = %s (%s), want matched with a withdrawn approval",
			resolved.Outcome, resolved.Reason)
	}
	approval, err = h.store.ApprovalByJob(ctx, *gated.JobID)
	if err != nil {
		t.Fatalf("get approval after resolve: %v", err)
	}
	if approval.State != store.ApprovalWithdrawn {
		t.Fatalf("approval state = %s, want withdrawn", approval.State)
	}

	// Rejoin inside the window: a duplicate of the resolved anchor (matched is
	// an acting outcome), so it is suppressed — no second approval, no churn.
	reopen2 := reopenTCP(t, addr, port)
	defer reopen2()
	h.advance(30 * time.Second)
	h.sweepN(t, reactive.DefaultHysteresis)
	follow := lastEvent(t, h.store)
	if follow.Outcome != store.OutcomeSuppressed || !strings.Contains(follow.Reason, "duplicate of event") {
		t.Fatalf("rejoin outcome = %s (%s), want suppressed as a duplicate", follow.Outcome, follow.Reason)
	}
}

// TestCheckDailyCapStaysPerRule pins the guardrail split the design keeps: the
// cooldown is per-target but the daily cap stays per-rule, so a second target
// joining the same rule's window is throttled — while its event row still
// exists with the rule named, the visible record a throttled match must leave.
func TestCheckDailyCapStaysPerRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	addrA, portA, closeA := tcpTarget(t)
	addrB, portB, closeB := tcpTarget(t)
	closeA()
	waitUntilRefused(t, addrA, portA)
	closeB()
	waitUntilRefused(t, addrB, portB)

	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "home", Kind: store.CheckTCP, IntervalS: 30,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: addrA, Port: portA},
			{Name: "nas", Address: addrB, Port: portB},
		},
	}); err != nil {
		t.Fatalf("create check: %v", err)
	}
	h.addRule(t, store.NewRule{
		Name:       "join",
		CELExpr:    `event.name == "home" && event.status == "firing"`,
		ActionKind: store.ActionShell,
		Action:     store.RuleAction{Argv: []string{"/bin/true"}},
		DailyCap:   1,
	})

	if _, err := h.engine.SweepChecks(ctx); err != nil {
		t.Fatalf("seed sweep: %v", err)
	}

	reopenA := reopenTCP(t, addrA, portA)
	defer reopenA()
	reopenB := reopenTCP(t, addrB, portB)
	defer reopenB()
	h.advance(30 * time.Second)
	h.sweepN(t, reactive.DefaultHysteresis)

	events, err := h.store.ListEvents(ctx, store.EventFilter{Source: reactive.SourceCheck})
	if err != nil {
		t.Fatalf("list check events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d check events, want 2 (both targets joined)", len(events))
	}
	byKey := map[string]store.Event{}
	for _, ev := range events {
		byKey[ev.DedupKey] = ev
	}
	first := byKey["check:home:laptop"]
	second := byKey["check:home:nas"]
	if first.Outcome != store.OutcomeFired {
		t.Fatalf("first join outcome = %s (%s), want fired", first.Outcome, first.Reason)
	}
	if second.Outcome != store.OutcomeSuppressed || second.MatchedRule != "join" {
		t.Fatalf("second join = outcome %s matched %q, want suppressed by the rule's cap with the rule named",
			second.Outcome, second.MatchedRule)
	}
	if !strings.Contains(second.Reason, "daily cap 1 reached") {
		t.Fatalf("second join reason = %q, want the cap to name itself", second.Reason)
	}
}

// TestCheckHTTPProbeRefusesLoopback drives the shipped (guarded) client through
// the sweep against a loopback test server: the dial-time guard refuses the
// connection, so the target is observed DOWN and never fires. The probe's
// GET-path is still exercised (the client's Do returns a refusal, not a panic),
// and the successful-GET path is covered by the internal
// TestCheckHTTPProbeReachesValidTarget.
func TestCheckHTTPProbeRefusesLoopback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "self", Kind: store.CheckHTTP, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "lo", URL: srv.URL + "/health", Expect: 200}},
	}); err != nil {
		t.Fatalf("create check: %v", err)
	}

	if raised, err := h.engine.SweepChecks(ctx); err != nil {
		t.Fatalf("seed sweep: %v", err)
	} else if raised != 0 {
		t.Fatalf("seed sweep raised %d events, want 0 (loopback refused -> down)", raised)
	}
	state, err := h.store.GetCheckState(ctx, mustCheckID(t, h, "self"), "lo")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.Status != store.CheckDown {
		t.Fatalf("loopback http target = %s, want down (guard refused the dial)", state.Status)
	}

	h.advance(30 * time.Second)
	if raised := h.sweepN(t, reactive.DefaultHysteresis); raised != 0 {
		t.Fatalf("steady loopback target raised %d events, want 0", raised)
	}
}

// mustCheckID looks a check's id back up by name, for reading its state rows.
func mustCheckID(t *testing.T, h *harness, name string) int64 {
	t.Helper()
	ctx := context.Background()
	c, err := h.store.GetCheck(ctx, name)
	if err != nil {
		t.Fatalf("get check %q: %v", name, err)
	}
	return c.ID
}

// TestCheckPingProbe drives the ping probe through a real subprocess: the
// loopback always answers on a host that can send ICMP, so the seed records up
// and steady state never emits.
func TestCheckPingProbe(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ping"); err != nil {
		t.Skip("no ping binary on this host")
	}
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	check, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "lo", Kind: store.CheckPing, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "me", Address: "127.0.0.1"}},
	})
	if err != nil {
		t.Fatalf("create ping check: %v", err)
	}

	raised, err := h.engine.SweepChecks(ctx)
	if err != nil {
		t.Fatalf("seed sweep: %v", err)
	}
	if raised != 0 {
		t.Fatalf("seed sweep raised %d events, want 0", raised)
	}
	state, err := h.store.GetCheckState(ctx, check.ID, "me")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.Status != store.CheckUp {
		t.Fatalf("ping probe state = %s, want up (the loopback answers)", state.Status)
	}

	raised = h.sweepN(t, reactive.DefaultHysteresis)
	if raised != 0 {
		t.Fatalf("steady ping raised %d events, want 0", raised)
	}
}

// assertLedger verifies an event's embedded hysteresis ledger: N homogeneous
// observations of the given status, consecutive running 1..N, newest last —
// the exact shape the "WHY THIS COUNTED" card renders.
func assertLedger(t *testing.T, ledgerJSON, status string) {
	t.Helper()
	var obs []struct {
		ObservedAt  string `json:"observed_at"`
		Status      string `json:"status"`
		Consecutive int    `json:"consecutive"`
	}
	if err := json.Unmarshal([]byte(ledgerJSON), &obs); err != nil {
		t.Fatalf("decode ledger %q: %v", ledgerJSON, err)
	}
	if len(obs) != reactive.DefaultHysteresis {
		t.Fatalf("ledger has %d observations, want %d", len(obs), reactive.DefaultHysteresis)
	}
	for i, o := range obs {
		if o.Status != status {
			t.Fatalf("ledger[%d] status = %q, want %q (homogeneous)", i, o.Status, status)
		}
		if o.Consecutive != i+1 {
			t.Fatalf("ledger[%d] consecutive = %d, want %d", i, o.Consecutive, i+1)
		}
		if o.ObservedAt == "" {
			t.Fatalf("ledger[%d] has no observed_at", i)
		}
	}
}
