package reactive

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/probe"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// TestCheckHTTPProbeReachesValidTarget exercises the successful-GET half of the
// check's http probe (the loopback-refusal sweep test covers the guarded path):
// with the client seam swapped for an unguarded one, a 200 target is up and a
// mismatched status is down. The dial guard itself is pinned in the probe
// package.
func TestCheckHTTPProbeReachesValidTarget(t *testing.T) {
	orig := probeHTTPClient
	defer func() { probeHTTPClient = orig }()
	probeHTTPClient = func() *http.Client { return &http.Client{Timeout: probeTimeout} }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	e := labelsEngine(t)
	ctx := context.Background()
	check := store.Check{Name: "web", Kind: store.CheckHTTP}

	up, _, err := e.probe(ctx, check, store.CheckTarget{Name: "w", URL: srv.URL + "/ok", Expect: 200})
	if err != nil || !up {
		t.Fatalf("200 target = %v, %v; want up", up, err)
	}
	up, _, err = e.probe(ctx, check, store.CheckTarget{Name: "w", URL: srv.URL + "/down", Expect: 200})
	if err != nil || up {
		t.Fatalf("503 target = %v, %v; want down", up, err)
	}
}

// labelsEngine opens a store and an engine for the label-builder tests. It is
// not the reactive_test harness: the label helpers are pure engine+store reads,
// so the heavier harness buys nothing here. opts mutate the engine's Options
// before it is built (the canary tests set Canary).
func labelsEngine(t *testing.T, opts ...func(*Options)) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      1,
		PollInterval: time.Hour,
		Logger:       slog.New(slog.DiscardHandler),
	})
	o := Options{
		Store:    st,
		Queue:    q,
		Deriver:  stubDeriver{},
		Notifier: notify.New(slog.New(slog.DiscardHandler), discardTransport{}),
		Clock:    func() time.Time { return time.Now().UTC() },
		Logger:   slog.New(slog.DiscardHandler),
	}
	for _, fn := range opts {
		fn(&o)
	}
	e, err := New(o)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

// TestCheckHTTPEventCarriesNoNetgroupLabel pins the http carve-out at the
// label-builder level: a URL target's liveness is not a location verdict, so
// its event labels carry no netgroup key, while an address target is classified
// (here into the "home" group, or "away" when no group matches).
func TestCheckHTTPEventCarriesNoNetgroupLabel(t *testing.T) {
	ctx := context.Background()
	e := labelsEngine(t)

	if _, err := e.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name:     "home",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	httpLabels, err := e.checkLabels(ctx, store.Check{Name: "web", Kind: store.CheckHTTP},
		store.CheckTarget{Name: "w", URL: "http://web.lan/health"})
	if err != nil {
		t.Fatalf("http labels: %v", err)
	}
	if _, ok := httpLabels["netgroup"]; ok {
		t.Fatalf("http event carries a netgroup label: %+v", httpLabels)
	}

	homeLabels, err := e.checkLabels(ctx, store.Check{Name: "at-home", Kind: store.CheckTCP},
		store.CheckTarget{Name: "laptop", Address: "192.168.1.50"})
	if err != nil {
		t.Fatalf("home labels: %v", err)
	}
	if homeLabels["netgroup"] != "home" {
		t.Fatalf("home target netgroup = %q, want home", homeLabels["netgroup"])
	}

	awayLabels, err := e.checkLabels(ctx, store.Check{Name: "roam", Kind: store.CheckTCP},
		store.CheckTarget{Name: "laptop", Address: "10.9.9.9"})
	if err != nil {
		t.Fatalf("away labels: %v", err)
	}
	if awayLabels["netgroup"] != "away" {
		t.Fatalf("unmatched target netgroup = %q, want away", awayLabels["netgroup"])
	}
}

// TestCheckNewKindsCarryNoNetgroupLabel pins the plan's non-IP rule: a dns
// name, a tls host:port, and a metric URL classify to no netgroup even when a
// netgroup covers the address literals they might embed. The shared spec's
// Location is what decides, so no read path parses a hostname or URL as an
// address.
func TestCheckNewKindsCarryNoNetgroupLabel(t *testing.T) {
	ctx := context.Background()
	e := labelsEngine(t)

	if _, err := e.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name:     "home",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("127.0.0.0/8")},
	}); err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	cases := []struct {
		check store.Check
		tgt   store.CheckTarget
	}{
		{store.Check{Name: "d", Kind: store.CheckDNS}, store.CheckTarget{Name: "web", DNSName: "192.168.1.50"}},
		{store.Check{Name: "t", Kind: store.CheckTLS}, store.CheckTarget{Name: "web", Address: "192.168.1.50", Port: 443, MinDays: 1}},
		{store.Check{Name: "m", Kind: store.CheckMetric}, store.CheckTarget{Name: "lag", URL: "http://192.168.1.50/metric", Op: "<", Value: 1}},
	}
	for _, tc := range cases {
		labels, err := e.checkLabels(ctx, tc.check, tc.tgt)
		if err != nil {
			t.Fatalf("%s labels: %v", tc.check.Kind, err)
		}
		if _, ok := labels["netgroup"]; ok {
			t.Fatalf("%s target carries a netgroup label: %+v", tc.check.Kind, labels)
		}
	}
}

// TestNetgroupForAwayOnMiss pins the shared miss fallback directly: a covered
// address names its group, and an uncovered one reads "away" (not an error), so
// every label builder treats a miss identically.
func TestNetgroupForAwayOnMiss(t *testing.T) {
	ctx := context.Background()
	e := labelsEngine(t)

	if _, err := e.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name:     "home",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")},
	}); err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	if group, err := e.netgroupFor(ctx, netip.MustParseAddr("192.168.1.50")); err != nil || group != "home" {
		t.Fatalf("netgroupFor(home) = (%q, %v), want (home, nil)", group, err)
	}
	if group, err := e.netgroupFor(ctx, netip.MustParseAddr("10.9.9.9")); err != nil || group != "away" {
		t.Fatalf("netgroupFor(miss) = (%q, %v), want (away, nil)", group, err)
	}
}

// listenLoopback binds a real listener on loopback and returns its address and
// port, so a tcp target answers up for the life of the test. The shipped sweep
// refuses the loopback class; a hermetic test has no other live target, so the
// helper holds the address-class guard open for the test (the guard's own
// behavior is pinned in internal/probe).
func listenLoopback(t *testing.T) (string, int) {
	t.Helper()
	t.Cleanup(probe.DisableAddrGuardForTest())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("listener port: %v", err)
	}
	return host, p
}

// TestSweepPersistsNetgroupAndDuration: a sweep writes each target's classified
// netgroup and its probe duration onto the state row, so the read paths never
// recompute. An http target's URL is not a location, so it stores "" — and its
// duration is recorded too, proven against a deliberately slow handler.
func TestSweepPersistsNetgroupAndDuration(t *testing.T) {
	orig := probeHTTPClient
	defer func() { probeHTTPClient = orig }()
	// The shipped dial guard refuses loopback; an unguarded client lets the
	// test server answer (the guard is pinned separately).
	probeHTTPClient = func() *http.Client { return &http.Client{Timeout: probeTimeout} }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := labelsEngine(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := e.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name:     "home",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	}); err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	// An address target: a live loopback listener covered by "home".
	host, port := listenLoopback(t)
	tcpCheck, err := e.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "laptop", Address: host, Port: port}},
	})
	if err != nil {
		t.Fatalf("create tcp check: %v", err)
	}
	// Pre-seed stale values so the sweep's write is observable: away -> home,
	// a sentinel duration -> the measured one.
	if err := e.store.SetCheckState(ctx, store.CheckState{
		CheckID: tcpCheck.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 3, Emitted: store.CheckUp, FirstSeen: now, LastChange: now,
		Netgroup: "away", LastDurationMS: 999999,
	}); err != nil {
		t.Fatalf("seed tcp state: %v", err)
	}
	if raised, err := e.sweepCheck(ctx, tcpCheck, now); err != nil {
		t.Fatalf("sweep tcp check: %v", err)
	} else if raised != 0 {
		t.Fatalf("steady tcp sweep raised %d events, want 0", raised)
	}
	tcpState, err := e.store.GetCheckState(ctx, tcpCheck.ID, "laptop")
	if err != nil {
		t.Fatalf("get tcp state: %v", err)
	}
	if tcpState.Netgroup != "home" {
		t.Fatalf("address target netgroup = %q, want home", tcpState.Netgroup)
	}
	if tcpState.LastDurationMS == 999999 {
		t.Fatalf("address target duration was not overwritten by the sweep: %d", tcpState.LastDurationMS)
	}

	// An http target: a URL, no address, so it stores no netgroup; the slow
	// handler makes its recorded duration unambiguously non-zero.
	httpCheck, err := e.store.CreateCheck(ctx, store.NewCheck{
		Name: "web-health", Kind: store.CheckHTTP, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "web", URL: srv.URL, Expect: 200}},
	})
	if err != nil {
		t.Fatalf("create http check: %v", err)
	}
	if raised, err := e.sweepCheck(ctx, httpCheck, now); err != nil {
		t.Fatalf("sweep http check: %v", err)
	} else if raised != 0 {
		t.Fatalf("http seed sweep raised %d events, want 0", raised)
	}
	httpState, err := e.store.GetCheckState(ctx, httpCheck.ID, "web")
	if err != nil {
		t.Fatalf("get http state: %v", err)
	}
	if httpState.Netgroup != "" {
		t.Fatalf("http target netgroup = %q, want the empty string", httpState.Netgroup)
	}
	if httpState.LastDurationMS < 10 {
		t.Fatalf("http target duration = %dms, want the ~30ms handler recorded", httpState.LastDurationMS)
	}
}

// closedLoopbackPort binds a loopback listener, closes it, and returns its
// address and port once a dial is refused, so a target reads down.
func closedLoopbackPort(t *testing.T) (string, int) {
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
	_ = ln.Close()

	addr := net.JoinHostPort(host, strconv.Itoa(p))
	deadline := time.Now().Add(2 * time.Second)
	d := net.Dialer{Timeout: 50 * time.Millisecond}
	for time.Now().Before(deadline) {
		conn, derr := d.Dial("tcp", addr)
		if derr != nil {
			return host, p
		}
		_ = conn.Close()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("port %s still accepts after the listener closed", addr)
	return "", 0
}

// TestSweepParallelProbingPreservesTransitions: probing a check's targets
// concurrently must not lose or duplicate a per-target transition. Six targets
// (three up, three down) are each one observation short of a declared
// transition; one sweep declares exactly six, one event per target, and each
// state row advances exactly once.
func TestSweepParallelProbingPreservesTransitions(t *testing.T) {
	e := labelsEngine(t)
	ctx := context.Background()
	now := time.Now().UTC()

	var targets []store.CheckTarget
	wantStatus := map[string]string{}
	for i := range 3 {
		host, port := listenLoopback(t)
		name := fmt.Sprintf("up-%d", i)
		targets = append(targets, store.CheckTarget{Name: name, Address: host, Port: port})
		wantStatus[name] = store.CheckUp
	}
	for i := range 3 {
		host, port := closedLoopbackPort(t)
		name := fmt.Sprintf("down-%d", i)
		targets = append(targets, store.CheckTarget{Name: name, Address: host, Port: port})
		wantStatus[name] = store.CheckDown
	}

	c, err := e.store.CreateCheck(ctx, store.NewCheck{
		Name: "multi", Kind: store.CheckTCP, IntervalS: 30, Targets: targets,
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	for _, tgt := range targets {
		status := wantStatus[tgt.Name]
		// Seed the streak one short with the opposite emitted flag, so the
		// sweep's one observation declares the transition.
		emitted := store.CheckDown
		if status == store.CheckDown {
			emitted = store.CheckUp
		}
		if err := e.store.SetCheckState(ctx, store.CheckState{
			CheckID: c.ID, TargetName: tgt.Name, Status: status,
			Consecutive: DefaultHysteresis - 1, Emitted: emitted,
			FirstSeen: now, LastChange: now,
		}); err != nil {
			t.Fatalf("seed state %s: %v", tgt.Name, err)
		}
	}

	raised, err := e.sweepCheck(ctx, c, now)
	if err != nil {
		t.Fatalf("sweepCheck: %v", err)
	}
	if raised != len(targets) {
		t.Fatalf("raised %d transitions, want %d (one per target)", raised, len(targets))
	}

	events, err := e.store.ListEvents(ctx, store.EventFilter{Source: SourceCheck})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	seen := map[string]int{}
	for _, ev := range events {
		seen[ev.DedupKey]++
	}
	for _, tgt := range targets {
		key := checkDedupKey("multi", tgt.Name)
		if seen[key] != 1 {
			t.Fatalf("target %s produced %d events, want exactly 1 (key %s)", tgt.Name, seen[key], key)
		}
		st, err := e.store.GetCheckState(ctx, c.ID, tgt.Name)
		if err != nil {
			t.Fatalf("get state %s: %v", tgt.Name, err)
		}
		if st.Emitted != wantStatus[tgt.Name] || st.Consecutive != DefaultHysteresis {
			t.Fatalf("state %s = emitted %q consecutive %d, want emitted %q consecutive %d",
				tgt.Name, st.Emitted, st.Consecutive, wantStatus[tgt.Name], DefaultHysteresis)
		}
	}
}

// seedDownTransition creates a one-target tcp check whose only target is
// unreachable and seeds its state one observation short of a declared down
// transition, so a sweep that applies state would emit exactly one resolve.
func seedDownTransition(t *testing.T, e *Engine) store.Check {
	t.Helper()
	ctx := context.Background()
	host, port := closedLoopbackPort(t)
	c, err := e.store.CreateCheck(ctx, store.NewCheck{
		Name: "edge", Kind: store.CheckTCP, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "edge", Address: host, Port: port}},
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	now := time.Now().UTC()
	if err := e.store.SetCheckState(ctx, store.CheckState{
		CheckID: c.ID, TargetName: "edge", Status: store.CheckDown,
		Consecutive: DefaultHysteresis - 1, Emitted: store.CheckUp,
		FirstSeen: now, LastChange: now,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return c
}

// TestCheckSweepCanaryUpBehavesNormally: with the canary reachable, a target
// that reads down still declares its transition.
func TestCheckSweepCanaryUpBehavesNormally(t *testing.T) {
	canaryHost, canaryPort := listenLoopback(t)
	canary := net.JoinHostPort(canaryHost, strconv.Itoa(canaryPort))
	e := labelsEngine(t, func(o *Options) { o.Canary = canary })

	ctx := context.Background()
	c := seedDownTransition(t, e)
	raised, err := e.sweepCheck(ctx, c, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweepCheck: %v", err)
	}
	if raised != 1 {
		t.Fatalf("raised %d transitions, want 1 with the canary up", raised)
	}
	st, err := e.store.GetCheckState(ctx, c.ID, "edge")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.Emitted != store.CheckDown {
		t.Fatalf("emitted = %q, want down", st.Emitted)
	}
}

// TestCheckSweepCanarySkipsWhenVantageDown: with the canary unreachable, the
// same would-be down transition emits nothing and the state does not advance —
// the daemon's own uplink loss is a missed observation, never a mass-down.
//
// Mutation check: drop the canary condition and this sweep emits the resolve
// and advances the streak, failing the assertions below.
func TestCheckSweepCanarySkipsWhenVantageDown(t *testing.T) {
	canaryHost, canaryPort := closedLoopbackPort(t)
	canary := net.JoinHostPort(canaryHost, strconv.Itoa(canaryPort))
	e := labelsEngine(t, func(o *Options) { o.Canary = canary })

	ctx := context.Background()
	c := seedDownTransition(t, e)
	raised, err := e.sweepCheck(ctx, c, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweepCheck: %v", err)
	}
	if raised != 0 {
		t.Fatalf("raised %d transitions, want 0 while the canary is down", raised)
	}

	st, err := e.store.GetCheckState(ctx, c.ID, "edge")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.Status != store.CheckDown || st.Consecutive != DefaultHysteresis-1 || st.Emitted != store.CheckUp {
		t.Fatalf("state advanced despite the canary: %+v", st)
	}
	events, err := e.store.ListEvents(ctx, store.EventFilter{Source: SourceCheck})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("emitted %d event(s) while the canary is down", len(events))
	}
}

// TestCheckSweepSnoozeSuppressesEmission: a snoozed check keeps probing and
// advancing its state (so hysteresis stays current) but emits nothing; once
// the snooze lapses, the same state declares the transition.
//
// Mutation check: drop the snoozed guard and the first sweep emits.
func TestCheckSweepSnoozeSuppressesEmission(t *testing.T) {
	e := labelsEngine(t)
	ctx := context.Background()
	now := time.Now().UTC()

	host, port := listenLoopback(t)
	if _, err := e.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "laptop", Address: host, Port: port}},
	}); err != nil {
		t.Fatalf("create check: %v", err)
	}
	c, err := e.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("get check: %v", err)
	}
	// One observation short of a declared up transition, emitted=down.
	if err := e.store.SetCheckState(ctx, store.CheckState{
		CheckID: c.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: DefaultHysteresis - 1, Emitted: store.CheckDown,
		FirstSeen: now, LastChange: now,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if err := e.store.SetCheckSnooze(ctx, "at-home", now.Add(time.Hour)); err != nil {
		t.Fatalf("set snooze: %v", err)
	}
	c, err = e.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("get snoozed check: %v", err)
	}

	raised, err := e.sweepCheck(ctx, c, now)
	if err != nil {
		t.Fatalf("snoozed sweepCheck: %v", err)
	}
	if raised != 0 {
		t.Fatalf("snoozed sweep raised %d transitions, want 0", raised)
	}
	st, err := e.store.GetCheckState(ctx, c.ID, "laptop")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.Consecutive != DefaultHysteresis || st.Emitted != store.CheckDown {
		t.Fatalf("snoozed state = consecutive %d emitted %q, want consecutive %d emitted down (advanced, not emitted)",
			st.Consecutive, st.Emitted, DefaultHysteresis)
	}

	// The snooze lapses: the next sweep declares the already-acquired state.
	if err := e.store.SetCheckSnooze(ctx, "at-home", time.Time{}); err != nil {
		t.Fatalf("clear snooze: %v", err)
	}
	c, err = e.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("get check after clear: %v", err)
	}
	raised, err = e.sweepCheck(ctx, c, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("unsnoozed sweepCheck: %v", err)
	}
	if raised != 1 {
		t.Fatalf("unsnoozed sweep raised %d transitions, want 1", raised)
	}
	st, err = e.store.GetCheckState(ctx, c.ID, "laptop")
	if err != nil {
		t.Fatalf("get state after clear: %v", err)
	}
	if st.Emitted != store.CheckUp {
		t.Fatalf("emitted after the snooze lapsed = %q, want up", st.Emitted)
	}
}

// seedAggregateCheck creates an opt-in aggregate tcp check whose first target
// answers up and whose second reads down, seeds both targets steady (so no
// per-target transition fires) and the aggregate's reserved state one
// observation short of an up transition, and returns the check.
func seedAggregateCheck(t *testing.T, e *Engine) store.Check {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	upHost, upPort := listenLoopback(t)
	downHost, downPort := closedLoopbackPort(t)
	c, err := e.store.CreateCheck(ctx, store.NewCheck{
		Name: "home", Kind: store.CheckTCP, IntervalS: 30,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: upHost, Port: upPort},
			{Name: "nas", Address: downHost, Port: downPort},
		},
		Aggregate: true, MinUp: 1,
	})
	if err != nil {
		t.Fatalf("create aggregate check: %v", err)
	}
	for name, status := range map[string]string{"laptop": store.CheckUp, "nas": store.CheckDown} {
		if err := e.store.SetCheckState(ctx, store.CheckState{
			CheckID: c.ID, TargetName: name, Status: status,
			Consecutive: DefaultHysteresis, Emitted: status,
			FirstSeen: now, LastChange: now,
		}); err != nil {
			t.Fatalf("seed %s state: %v", name, err)
		}
	}
	if err := e.store.SetCheckState(ctx, store.CheckState{
		CheckID: c.ID, TargetName: store.AggregateTargetName, Status: store.CheckUp,
		Consecutive: DefaultHysteresis - 1, Emitted: store.CheckDown,
		FirstSeen: now, LastChange: now,
	}); err != nil {
		t.Fatalf("seed aggregate state: %v", err)
	}
	return c
}

// TestSweepAggregateEmitsOneEventForPresentSet: an aggregate check with two
// targets and min_up=1 declares exactly one event when one target is up and the
// other down, keyed check:<check>:aggregate, carrying the sorted present set as
// hosts and the up/total counts as labels — and, because both targets are
// steady, no per-target event.
func TestSweepAggregateEmitsOneEventForPresentSet(t *testing.T) {
	e := labelsEngine(t)
	ctx := context.Background()
	now := time.Now().UTC()

	c := seedAggregateCheck(t, e)
	raised, err := e.sweepCheck(ctx, c, now)
	if err != nil {
		t.Fatalf("sweepCheck: %v", err)
	}
	if raised != 1 {
		t.Fatalf("raised %d transitions, want 1 (the aggregate only)", raised)
	}

	events, err := e.store.ListEvents(ctx, store.EventFilter{Source: SourceCheck})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d check events, want exactly 1 aggregate event", len(events))
	}
	ev := events[0]
	if ev.DedupKey != "check:home:aggregate" {
		t.Fatalf("dedup key = %q, want check:home:aggregate", ev.DedupKey)
	}
	var norm Event
	if err := json.Unmarshal([]byte(ev.Normalized), &norm); err != nil {
		t.Fatalf("decode normalized event: %v", err)
	}
	if norm.Status != StatusFiring || norm.Host != "" {
		t.Fatalf("aggregate event = status %q host %q, want firing with an empty host", norm.Status, norm.Host)
	}
	if norm.Labels["aggregate"] != "true" || norm.Labels["up"] != "1" || norm.Labels["total"] != "2" {
		t.Fatalf("aggregate labels = %+v, want aggregate=true up=1 total=2", norm.Labels)
	}
	if len(norm.Hosts) != 1 || norm.Hosts[0] != "laptop" {
		t.Fatalf("aggregate hosts = %v, want [laptop] (the one target up)", norm.Hosts)
	}

	agg, err := e.store.GetCheckState(ctx, c.ID, store.AggregateTargetName)
	if err != nil {
		t.Fatalf("get aggregate state: %v", err)
	}
	if agg.Emitted != store.CheckUp || agg.Consecutive != DefaultHysteresis {
		t.Fatalf("aggregate state = %+v, want emitted up at the hysteresis window", agg)
	}
}

// durationLabelMS reads the duration_ms label off the check event keyed by
// dedupKey, decoding the stored normalized event the pipeline wrote.
func durationLabelMS(t *testing.T, e *Engine, dedupKey string) int64 {
	t.Helper()
	events, err := e.store.ListEvents(context.Background(), store.EventFilter{Source: SourceCheck})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, ev := range events {
		if ev.DedupKey != dedupKey {
			continue
		}
		var norm Event
		if err := json.Unmarshal([]byte(ev.Normalized), &norm); err != nil {
			t.Fatalf("decode normalized event: %v", err)
		}
		ms, err := strconv.ParseInt(norm.Labels["duration_ms"], 10, 64)
		if err != nil {
			t.Fatalf("event %s duration_ms label = %q, want a number", dedupKey, norm.Labels["duration_ms"])
		}
		return ms
	}
	t.Fatalf("no check event keyed %s", dedupKey)
	return 0
}

// TestCheckTransitionCarriesDurationLabel: a declared transition's event carries
// the probe's measured duration under labels["duration_ms"], so a rule can watch
// "up but slow" (S3) without a second probe. A slow http handler's event reads
// high and a loopback tcp dial reads low.
//
// Mutation check: drop the label from emitCheckTransition and durationLabelMS
// finds an empty value, failing every assertion below.
func TestCheckTransitionCarriesDurationLabel(t *testing.T) {
	orig := probeHTTPClient
	defer func() { probeHTTPClient = orig }()
	probeHTTPClient = func() *http.Client { return &http.Client{Timeout: probeTimeout} }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(40 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := labelsEngine(t)
	ctx := context.Background()
	now := time.Now().UTC()

	host, port := listenLoopback(t)
	fast, err := e.store.CreateCheck(ctx, store.NewCheck{
		Name: "fast", Kind: store.CheckTCP, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "t", Address: host, Port: port}},
	})
	if err != nil {
		t.Fatalf("create fast check: %v", err)
	}
	slow, err := e.store.CreateCheck(ctx, store.NewCheck{
		Name: "slow", Kind: store.CheckHTTP, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "w", URL: srv.URL, Expect: 200}},
	})
	if err != nil {
		t.Fatalf("create slow check: %v", err)
	}
	seed := func(id int64, target string) {
		t.Helper()
		if err := e.store.SetCheckState(ctx, store.CheckState{
			CheckID: id, TargetName: target, Status: store.CheckUp,
			Consecutive: DefaultHysteresis - 1, Emitted: store.CheckDown,
			FirstSeen: now, LastChange: now,
		}); err != nil {
			t.Fatalf("seed %s state: %v", target, err)
		}
	}
	seed(fast.ID, "t")
	seed(slow.ID, "w")

	for _, c := range []store.Check{fast, slow} {
		if raised, err := e.sweepCheck(ctx, c, now); err != nil || raised != 1 {
			t.Fatalf("sweep %s = (%d, %v), want one declared transition", c.Name, raised, err)
		}
	}

	fastMS := durationLabelMS(t, e, checkDedupKey("fast", "t"))
	slowMS := durationLabelMS(t, e, checkDedupKey("slow", "w"))
	if slowMS < 30 {
		t.Fatalf("slow transition duration_ms = %d, want the ~40ms probe recorded", slowMS)
	}
	if fastMS >= slowMS {
		t.Fatalf("fast duration_ms = %d, slow = %d; want the loopback dial the lower", fastMS, slowMS)
	}
}

// TestSweepAggregateSnoozeSuppressesEmission: a snooze advances the aggregate's
// state but withholds its event, exactly as for a target; the transition
// declares once the snooze lapses.
//
// Mutation check: drop the snoozed guard from the aggregate branch and the
// first sweep emits, failing the "want 0" assertions.
func TestSweepAggregateSnoozeSuppressesEmission(t *testing.T) {
	e := labelsEngine(t)
	ctx := context.Background()
	now := time.Now().UTC()

	c := seedAggregateCheck(t, e)
	if err := e.store.SetCheckSnooze(ctx, c.Name, now.Add(time.Hour)); err != nil {
		t.Fatalf("set snooze: %v", err)
	}
	c, err := e.store.GetCheck(ctx, c.Name)
	if err != nil {
		t.Fatalf("get snoozed check: %v", err)
	}

	raised, err := e.sweepCheck(ctx, c, now)
	if err != nil {
		t.Fatalf("snoozed sweepCheck: %v", err)
	}
	if raised != 0 {
		t.Fatalf("snoozed aggregate sweep raised %d transitions, want 0", raised)
	}
	events, err := e.store.ListEvents(ctx, store.EventFilter{Source: SourceCheck})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("snoozed sweep emitted %d event(s), want 0", len(events))
	}
	agg, err := e.store.GetCheckState(ctx, c.ID, store.AggregateTargetName)
	if err != nil {
		t.Fatalf("get aggregate state: %v", err)
	}
	if agg.Consecutive != DefaultHysteresis || agg.Emitted != store.CheckDown {
		t.Fatalf("snoozed aggregate = consecutive %d emitted %q, want it advanced but not emitted",
			agg.Consecutive, agg.Emitted)
	}

	// The snooze lapses: the next sweep declares the already-acquired state.
	if err := e.store.SetCheckSnooze(ctx, c.Name, time.Time{}); err != nil {
		t.Fatalf("clear snooze: %v", err)
	}
	c, err = e.store.GetCheck(ctx, c.Name)
	if err != nil {
		t.Fatalf("get check after clear: %v", err)
	}
	raised, err = e.sweepCheck(ctx, c, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("unsnoozed sweepCheck: %v", err)
	}
	if raised != 1 {
		t.Fatalf("unsnoozed aggregate sweep raised %d transitions, want 1", raised)
	}
	agg, err = e.store.GetCheckState(ctx, c.ID, store.AggregateTargetName)
	if err != nil {
		t.Fatalf("get aggregate state after clear: %v", err)
	}
	if agg.Emitted != store.CheckUp {
		t.Fatalf("aggregate emitted after the snooze lapsed = %q, want up", agg.Emitted)
	}
}
