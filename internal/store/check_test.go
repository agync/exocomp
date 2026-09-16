package store_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

func checkFixture() store.NewCheck {
	return store.NewCheck{
		Name:      "at-home",
		Kind:      store.CheckTCP,
		IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: "192.168.1.50", Port: 22},
			{Name: "nas", Address: "192.168.1.60", Port: 22},
		},
	}
}

func TestCheckCRUDAndState(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateCheck(ctx, checkFixture())
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	if !created.Enabled || created.IntervalS != 300 || len(created.Targets) != 2 {
		t.Fatalf("created check = %+v", created)
	}

	got, err := st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("get check: %v", err)
	}
	if got.ID != created.ID || got.Targets[1].Address != "192.168.1.60" {
		t.Fatalf("get check = %+v", got)
	}

	enabled, err := st.ListChecks(ctx, true)
	if err != nil {
		t.Fatalf("list enabled checks: %v", err)
	}
	if len(enabled) != 1 {
		t.Fatalf("listed %d enabled checks, want 1", len(enabled))
	}

	// State upsert: first_seen sticks, last_change follows the sweep's writes.
	firstSeen := time.Now().Add(-time.Hour)
	lastChange := time.Now()
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: created.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 1, Emitted: store.CheckUp,
		FirstSeen: firstSeen, LastChange: lastChange,
	}); err != nil {
		t.Fatalf("set state: %v", err)
	}
	later := time.Now()
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: created.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 3, Emitted: store.CheckUp,
		FirstSeen: firstSeen, LastChange: later,
	}); err != nil {
		t.Fatalf("set state again: %v", err)
	}
	state, err := st.GetCheckState(ctx, created.ID, "laptop")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.Consecutive != 3 {
		t.Fatalf("state = %+v, want consecutive 3 after the upsert", state)
	}
	if state.FirstSeen.UnixMilli() != firstSeen.UTC().UnixMilli() {
		t.Fatalf("first_seen moved on upsert: %+v", state)
	}
	if state.LastChange.UnixMilli() != later.UTC().UnixMilli() {
		t.Fatalf("last_change did not follow the upsert: %+v", state)
	}

	all, err := st.ListCheckStates(ctx, created.ID)
	if err != nil {
		t.Fatalf("list states: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("listed %d states, want 1", len(all))
	}

	// Disable: definition survives, the sweep's enabled view stops seeing it.
	if err := st.SetCheckEnabled(ctx, "at-home", false); err != nil {
		t.Fatalf("disable check: %v", err)
	}
	enabled, err = st.ListChecks(ctx, true)
	if err != nil {
		t.Fatalf("list enabled checks: %v", err)
	}
	if len(enabled) != 0 {
		t.Fatalf("listed %d enabled checks after disabling, want 0", len(enabled))
	}
	if _, err := st.GetCheck(ctx, "at-home"); err != nil {
		t.Fatalf("disabled check should still read: %v", err)
	}

	// Delete: check gone, and its state rows cascade away with it.
	if err := st.DeleteCheck(ctx, "at-home"); err != nil {
		t.Fatalf("delete check: %v", err)
	}
	if _, err := st.GetCheck(ctx, "at-home"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get deleted check: err = %v, want ErrNotFound", err)
	}
	orphans, err := st.ListCheckStates(ctx, created.ID)
	if err != nil {
		t.Fatalf("list states after delete: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("delete left %d orphaned state rows; the FK must cascade", len(orphans))
	}
}

// TestCheckStateNetgroupRoundTrip: the stored netgroup survives the state
// upsert both ways — a classified value (home/away) is overwritten on the next
// observation, so the read paths always see the latest classification.
func TestCheckStateNetgroupRoundTrip(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateCheck(ctx, checkFixture())
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	now := time.Now().UTC()
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: created.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 1, Emitted: store.CheckUp, FirstSeen: now, LastChange: now,
		Netgroup: "home",
	}); err != nil {
		t.Fatalf("set state: %v", err)
	}
	got, err := st.GetCheckState(ctx, created.ID, "laptop")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if got.Netgroup != "home" {
		t.Fatalf("netgroup = %q, want home", got.Netgroup)
	}

	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: created.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 2, Emitted: store.CheckUp, FirstSeen: now, LastChange: now,
		Netgroup: "away",
	}); err != nil {
		t.Fatalf("set state again: %v", err)
	}
	got, err = st.GetCheckState(ctx, created.ID, "laptop")
	if err != nil {
		t.Fatalf("get state after update: %v", err)
	}
	if got.Netgroup != "away" {
		t.Fatalf("netgroup after update = %q, want away", got.Netgroup)
	}
}

// TestCheckStateDurationRoundTrip: the last probe's duration persists on the
// state row and follows the upsert, so a slow target's reading is not stale.
func TestCheckStateDurationRoundTrip(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateCheck(ctx, checkFixture())
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	now := time.Now().UTC()
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: created.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 1, Emitted: store.CheckUp, FirstSeen: now, LastChange: now,
		LastDurationMS: 12,
	}); err != nil {
		t.Fatalf("set state: %v", err)
	}
	got, err := st.GetCheckState(ctx, created.ID, "laptop")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if got.LastDurationMS != 12 {
		t.Fatalf("last_duration_ms = %d, want 12", got.LastDurationMS)
	}

	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: created.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 2, Emitted: store.CheckUp, FirstSeen: now, LastChange: now,
		LastDurationMS: 340,
	}); err != nil {
		t.Fatalf("set state again: %v", err)
	}
	got, err = st.GetCheckState(ctx, created.ID, "laptop")
	if err != nil {
		t.Fatalf("get state after update: %v", err)
	}
	if got.LastDurationMS != 340 {
		t.Fatalf("last_duration_ms after update = %d, want 340", got.LastDurationMS)
	}
}

// TestSetCheckSnooze: a snooze stores its expiry and reads back on the check;
// the zero time clears it, and an unknown name is refused.
func TestSetCheckSnooze(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	if _, err := st.CreateCheck(ctx, checkFixture()); err != nil {
		t.Fatalf("create check: %v", err)
	}
	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	if err := st.SetCheckSnooze(ctx, "at-home", until); err != nil {
		t.Fatalf("set snooze: %v", err)
	}
	got, err := st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("get check: %v", err)
	}
	if !got.SnoozedUntil.Equal(until) {
		t.Fatalf("snoozed_until = %v, want %v", got.SnoozedUntil, until)
	}

	if err := st.SetCheckSnooze(ctx, "at-home", time.Time{}); err != nil {
		t.Fatalf("clear snooze: %v", err)
	}
	got, err = st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("get check after clear: %v", err)
	}
	if !got.SnoozedUntil.IsZero() {
		t.Fatalf("snoozed_until = %v after clear, want the zero time", got.SnoozedUntil)
	}

	if err := st.SetCheckSnooze(ctx, "missing", until); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetCheckSnooze(missing) = %v, want ErrNotFound", err)
	}
}

func TestCheckValidationRefusals(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		mutate  func(*store.NewCheck)
		wantErr string
	}{
		{"bad name", func(n *store.NewCheck) { n.Name = "bad name!" }, "must match"},
		{"bad kind", func(n *store.NewCheck) { n.Kind = "snmp" }, "kind"},
		{"zero interval", func(n *store.NewCheck) { n.IntervalS = 0 }, "interval_s must be positive"},
		{"negative interval", func(n *store.NewCheck) { n.IntervalS = -30 }, "interval_s must be positive"},
		{"no targets", func(n *store.NewCheck) { n.Targets = nil }, "at least one target"},
		{"duplicate target name", func(n *store.NewCheck) {
			n.Targets = []store.CheckTarget{
				{Name: "dup", Address: "192.168.1.1", Port: 22},
				{Name: "dup", Address: "192.168.1.2", Port: 22},
			}
		}, "duplicated target name"},
		{"target name charset", func(n *store.NewCheck) {
			n.Targets = []store.CheckTarget{{Name: "bad/name", Address: "192.168.1.1", Port: 22}}
		}, "must match"},

		{"tcp address is a hostname", func(n *store.NewCheck) {
			n.Targets = []store.CheckTarget{{Name: "web", Address: "web.lan", Port: 443}}
		}, "not an IP literal"},
		{"tcp port zero", func(n *store.NewCheck) {
			n.Targets = []store.CheckTarget{{Name: "web", Address: "192.168.1.1", Port: 0}}
		}, "port 0 out of range"},
		{"tcp port too high", func(n *store.NewCheck) {
			n.Targets = []store.CheckTarget{{Name: "web", Address: "192.168.1.1", Port: 70000}}
		}, "port 70000 out of range"},

		{"ping address is a hostname", func(n *store.NewCheck) {
			n.Kind = store.CheckPing
			n.Targets = []store.CheckTarget{{Name: "gate", Address: "gateway.local"}}
		}, "not an IP literal"},
		{"ping carries a port", func(n *store.NewCheck) {
			n.Kind = store.CheckPing
			n.Targets = []store.CheckTarget{{Name: "gate", Address: "192.168.1.1", Port: 1}}
		}, "ping probes an address"},

		{"http url unparseable", func(n *store.NewCheck) {
			n.Kind = store.CheckHTTP
			n.Targets = []store.CheckTarget{{Name: "web", URL: "http://[::1"}}
		}, "does not parse"},
		{"http url without a host", func(n *store.NewCheck) {
			n.Kind = store.CheckHTTP
			n.Targets = []store.CheckTarget{{Name: "web", URL: "http://"}}
		}, "http(s) URL with a host"},
		{"http scheme refused", func(n *store.NewCheck) {
			n.Kind = store.CheckHTTP
			n.Targets = []store.CheckTarget{{Name: "web", URL: "ftp://web.lan/file"}}
		}, "http(s) URL with a host"},
		{"http expect out of range", func(n *store.NewCheck) {
			n.Kind = store.CheckHTTP
			n.Targets = []store.CheckTarget{{Name: "web", URL: "http://web.lan/health", Expect: 42}}
		}, "expect 42 out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := checkFixture()
			tc.mutate(&n)
			_, err := st.CreateCheck(ctx, n)
			if err == nil {
				t.Fatalf("CreateCheck succeeded, want refusal %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if !errors.Is(err, store.ErrValidation) {
				t.Fatalf("err = %v, want it to carry ErrValidation (operator input, not a store failure)", err)
			}
		})
	}
}

func TestCheckDuplicateName(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	if _, err := st.CreateCheck(ctx, checkFixture()); err != nil {
		t.Fatalf("create check: %v", err)
	}
	if _, err := st.CreateCheck(ctx, checkFixture()); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("second create: err = %v, want ErrDuplicate", err)
	}
}

// TestCountCheckStatesByStatus is the dashboard card's count: it tallies state
// rows by status across checks in one query — one target up, one down — and
// reads zero (not an error) for a status nothing holds. A disabled check's
// frozen state rows are not the sweep's business, so they do not count.
func TestCountCheckStatesByStatus(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	c, err := st.CreateCheck(ctx, checkFixture())
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	now := time.Now().UTC()
	for _, tc := range []struct {
		target, status string
	}{
		{"laptop", store.CheckUp},
		{"nas", store.CheckDown},
	} {
		if err := st.SetCheckState(ctx, store.CheckState{
			CheckID: c.ID, TargetName: tc.target, Status: tc.status,
			Consecutive: 1, Emitted: tc.status, FirstSeen: now, LastChange: now,
		}); err != nil {
			t.Fatalf("set state %s: %v", tc.target, err)
		}
	}

	// A disabled check with a frozen up-state row: the count must not include
	// it, because the sweep no longer probes it.
	parked, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "parked", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "old", Address: "10.0.0.9", Port: 22}},
	})
	if err != nil {
		t.Fatalf("create parked check: %v", err)
	}
	if err := st.SetCheckEnabled(ctx, "parked", false); err != nil {
		t.Fatalf("disable parked check: %v", err)
	}
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: parked.ID, TargetName: "old", Status: store.CheckUp,
		Consecutive: 3, Emitted: store.CheckUp, FirstSeen: now, LastChange: now,
	}); err != nil {
		t.Fatalf("set parked state: %v", err)
	}

	up, err := st.CountCheckStatesByStatus(ctx, store.CheckUp)
	if err != nil {
		t.Fatalf("count up states: %v", err)
	}
	if up != 1 {
		t.Fatalf("up count = %d, want 1 (the disabled check's frozen up must not count)", up)
	}
	down, err := st.CountCheckStatesByStatus(ctx, store.CheckDown)
	if err != nil {
		t.Fatalf("count down states: %v", err)
	}
	if down != 1 {
		t.Fatalf("down count = %d, want 1", down)
	}
	none, err := st.CountCheckStatesByStatus(ctx, "unobserved")
	if err != nil {
		t.Fatalf("count unknown status: %v", err)
	}
	if none != 0 {
		t.Fatalf("unknown status count = %d, want 0", none)
	}
}

// TestParseCheckTargets pins the one parser the CLI (strict) and the checks
// page (blank-skipping) share: the per-kind target shape, the default-kind
// error, and the skipBlank leniency.
func TestParseCheckTargets(t *testing.T) {
	tcp, err := store.ParseCheckTargets(store.CheckTCP,
		[]string{"laptop=192.168.1.50:22", "nas=192.168.1.60:22"}, false)
	if err != nil {
		t.Fatalf("parse tcp: %v", err)
	}
	if len(tcp) != 2 || tcp[0].Address != "192.168.1.50" || tcp[0].Port != 22 ||
		tcp[1].Address != "192.168.1.60" {
		t.Fatalf("tcp targets = %+v", tcp)
	}

	ping, err := store.ParseCheckTargets(store.CheckPing, []string{"gate=192.168.1.1"}, false)
	if err != nil {
		t.Fatalf("parse ping: %v", err)
	}
	if len(ping) != 1 || ping[0].Address != "192.168.1.1" || ping[0].Port != 0 {
		t.Fatalf("ping targets = %+v", ping)
	}

	http, err := store.ParseCheckTargets(store.CheckHTTP, []string{"web=http://web.lan/health"}, false)
	if err != nil {
		t.Fatalf("parse http: %v", err)
	}
	if len(http) != 1 || http[0].URL != "http://web.lan/health" {
		t.Fatalf("http targets = %+v", http)
	}

	if _, err := store.ParseCheckTargets("snmp", []string{"a=1.2.3.4:1"}, false); err == nil ||
		!strings.Contains(err.Error(), "--kind must be") {
		t.Fatalf("unknown kind error = %v, want the CLI's --kind refusal", err)
	}

	// Strict: a blank target is an error, not a skip.
	if _, err := store.ParseCheckTargets(store.CheckTCP, []string{"  "}, false); err == nil {
		t.Fatal("strict mode accepted a blank target")
	}
	// Web leniency: blanks are trimmed and skipped.
	web, err := store.ParseCheckTargets(store.CheckTCP,
		[]string{"laptop=192.168.1.50:22", "  ", "", "nas=192.168.1.60:22"}, true)
	if err != nil {
		t.Fatalf("parse web targets: %v", err)
	}
	if len(web) != 2 {
		t.Fatalf("web targets = %+v, want the two non-blank ones", web)
	}
}

// TestParseCheckTargetsNewKinds pins the parser's three added shapes: tls is
// host:port split into Address+Port, dns is a bare hostname, metric is a URL
// whose Op/Value a surface must still set (they are not parsed from the spec).
func TestParseCheckTargetsNewKinds(t *testing.T) {
	tls, err := store.ParseCheckTargets(store.CheckTLS, []string{"web=web.lan:443"}, false)
	if err != nil {
		t.Fatalf("parse tls: %v", err)
	}
	if len(tls) != 1 || tls[0].Address != "web.lan" || tls[0].Port != 443 {
		t.Fatalf("tls targets = %+v", tls)
	}

	dns, err := store.ParseCheckTargets(store.CheckDNS, []string{"web=web.lan"}, false)
	if err != nil {
		t.Fatalf("parse dns: %v", err)
	}
	if len(dns) != 1 || dns[0].DNSName != "web.lan" || dns[0].Address != "" {
		t.Fatalf("dns targets = %+v", dns)
	}

	metric, err := store.ParseCheckTargets(store.CheckMetric, []string{"lag=http://web.lan/metric"}, false)
	if err != nil {
		t.Fatalf("parse metric: %v", err)
	}
	if len(metric) != 1 || metric[0].URL != "http://web.lan/metric" || metric[0].Op != "" {
		t.Fatalf("metric targets = %+v", metric)
	}

	if _, err := store.ParseCheckTargets(store.CheckTLS, []string{"web=web.lan"}, false); err == nil {
		t.Fatal("tls target without a port was accepted")
	}
}

// TestCheckNewKindsValidation drives the per-kind validation the new kinds
// delegate to the shared probe spec: a well-formed dns/tls/metric target is
// accepted, and each kind's incoherent shape is refused with ErrValidation.
func TestCheckNewKindsValidation(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	valid := []store.NewCheck{
		{Name: "dns-ok", Kind: store.CheckDNS, IntervalS: 300,
			Targets: []store.CheckTarget{{Name: "web", DNSName: "web.lan", ExpectAddr: "10.0.0.5"}}},
		{Name: "tls-ok", Kind: store.CheckTLS, IntervalS: 300,
			Targets: []store.CheckTarget{{Name: "web", Address: "web.lan", Port: 443, MinDays: 14}}},
		{Name: "metric-ok", Kind: store.CheckMetric, IntervalS: 300,
			Targets: []store.CheckTarget{{Name: "lag", URL: "http://web.lan/m", Op: "<", Value: 1}}},
	}
	for _, n := range valid {
		created, err := st.CreateCheck(ctx, n)
		if err != nil {
			t.Fatalf("create %s: %v", n.Name, err)
		}
		if created.Kind != n.Kind || len(created.Targets) != 1 {
			t.Fatalf("created %s = %+v", n.Name, created)
		}
	}

	refused := []struct {
		name    string
		check   store.NewCheck
		wantErr string
	}{
		{"dns without a name", store.NewCheck{Name: "d", Kind: store.CheckDNS, IntervalS: 300,
			Targets: []store.CheckTarget{{Name: "web"}}}, "dns needs a name"},
		{"tls without a port", store.NewCheck{Name: "t", Kind: store.CheckTLS, IntervalS: 300,
			Targets: []store.CheckTarget{{Name: "web", Address: "web.lan"}}}, "out of range"},
		{"tls negative days", store.NewCheck{Name: "t", Kind: store.CheckTLS, IntervalS: 300,
			Targets: []store.CheckTarget{{Name: "web", Address: "web.lan", Port: 443, MinDays: -1}}}, "min_days"},
		{"metric without an op", store.NewCheck{Name: "m", Kind: store.CheckMetric, IntervalS: 300,
			Targets: []store.CheckTarget{{Name: "lag", URL: "http://web.lan/m"}}}, "metric op"},
		{"metric with a bad op", store.NewCheck{Name: "m", Kind: store.CheckMetric, IntervalS: 300,
			Targets: []store.CheckTarget{{Name: "lag", URL: "http://web.lan/m", Op: "~"}}}, "metric op"},
		{"metric without a url", store.NewCheck{Name: "m", Kind: store.CheckMetric, IntervalS: 300,
			Targets: []store.CheckTarget{{Name: "lag", Op: "<", Value: 1}}}, "url"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.CreateCheck(ctx, tc.check)
			if err == nil {
				t.Fatalf("CreateCheck succeeded, want refusal %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if !errors.Is(err, store.ErrValidation) {
				t.Fatalf("err = %v, want it to carry ErrValidation", err)
			}
		})
	}
}

func TestCheckPingRefusedWhenBinaryMissing(t *testing.T) {
	// Serial on purpose: t.Setenv must not race the package's parallel tests.
	st := openReactive(t)
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()

	_, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "ping-gate", Kind: store.CheckPing, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "lo", Address: "127.0.0.1"}},
	})
	if err == nil || !strings.Contains(err.Error(), "ping binary not on PATH") {
		t.Fatalf("err = %v, want a ping-not-on-PATH refusal", err)
	}
	// A live-loopback probe failure is a real store/infra condition, not an
	// operator typo: it must NOT carry the ErrValidation sentinel.
	if errors.Is(err, store.ErrValidation) {
		t.Fatalf("err = %v must not carry ErrValidation; a missing ping capability is a store error", err)
	}
}

func TestCheckPingAddsWhenHostCanICMP(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ping"); err != nil {
		t.Skip("no ping binary on this host; the capability probe cannot pass")
	}
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "ping-gate", Kind: store.CheckPing, IntervalS: 30,
		Targets: []store.CheckTarget{{Name: "gate", Address: "192.168.1.1"}},
	})
	if err != nil {
		t.Fatalf("create ping check on a ping-capable host: %v", err)
	}
	if created.Kind != store.CheckPing {
		t.Fatalf("created kind = %q", created.Kind)
	}
}

// TestCheckUpdatePreservesState is the whole point of `check update`: a
// re-point that keeps the target NAME leaves its check_state row untouched, so
// the sweep can flip that kept row and emit a real transition. If the update
// wiped or re-keyed state, the re-point could only cold-start (seeding without
// emitting) exactly like rm+add did.
func TestCheckUpdatePreservesState(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	firstSeen := time.Now().Add(-time.Hour)
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: created.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 3, Emitted: store.CheckUp,
		FirstSeen: firstSeen, LastChange: time.Now(),
		RecentObservations: `[{"observed_at":"2024-05-01T12:00:00Z","status":"up","consecutive":3}]`,
	}); err != nil {
		t.Fatalf("set state: %v", err)
	}

	updated, err := st.UpdateCheck(ctx, "at-home",
		[]store.CheckTarget{{Name: "laptop", Address: "127.0.0.1", Port: 2222}}, 30)
	if err != nil {
		t.Fatalf("update check: %v", err)
	}
	if updated.Kind != store.CheckTCP || updated.IntervalS != 30 || len(updated.Targets) != 1 ||
		updated.Targets[0].Address != "127.0.0.1" || updated.Targets[0].Port != 2222 {
		t.Fatalf("updated = %+v", updated)
	}

	kept, err := st.GetCheckState(ctx, created.ID, "laptop")
	if err != nil {
		t.Fatalf("state after update: %v", err)
	}
	if kept.Consecutive != 3 || kept.Emitted != store.CheckUp ||
		kept.FirstSeen.UnixMilli() != firstSeen.UTC().UnixMilli() {
		t.Fatalf("state = %+v, want it preserved across the update", kept)
	}
}

// TestCheckUpdatePrunesDroppedTargetState: a re-point that drops a target must
// delete its check_state row. A kept NAME keeps its row (the point of update
// over rm+add), but a dropped one is a target the check no longer probes — its
// stale state would otherwise be read by every state-row consumer, including
// the /metrics check gauges.
func TestCheckUpdatePrunesDroppedTargetState(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateCheck(ctx, checkFixture())
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	for _, name := range []string{"laptop", "nas"} {
		if err := st.SetCheckState(ctx, store.CheckState{
			CheckID: created.ID, TargetName: name, Status: store.CheckUp,
			Consecutive: 3, Emitted: store.CheckUp,
			FirstSeen: time.Now(), LastChange: time.Now(),
		}); err != nil {
			t.Fatalf("set state %s: %v", name, err)
		}
	}

	if _, err := st.UpdateCheck(ctx, "at-home",
		[]store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}}, 300); err != nil {
		t.Fatalf("update check: %v", err)
	}

	if _, err := st.GetCheckState(ctx, created.ID, "laptop"); err != nil {
		t.Fatalf("kept target's state was pruned: %v", err)
	}
	if _, err := st.GetCheckState(ctx, created.ID, "nas"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dropped target's state err = %v, want ErrNotFound", err)
	}
}

// TestCheckUpdateValidationRefusals mirrors CreateCheck's shape rules but
// against the STORED check's kind: a kind is read from the row and never
// replaced, so an http-shaped target on a stored tcp check (the only way to
// attempt a kind change here) is refused. A refused update must leave the
// stored row untouched.
func TestCheckUpdateValidationRefusals(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	if _, err := st.CreateCheck(ctx, checkFixture()); err != nil {
		t.Fatalf("create check: %v", err)
	}

	cases := []struct {
		name      string
		targets   []store.CheckTarget
		intervalS int64
		wantErr   string
	}{
		{"no targets", nil, 300, "at least one target"},
		{"duplicate target name", []store.CheckTarget{
			{Name: "dup", Address: "192.168.1.1", Port: 22},
			{Name: "dup", Address: "192.168.1.2", Port: 22},
		}, 300, "duplicated target name"},
		{"target name charset", []store.CheckTarget{{Name: "bad/name", Address: "192.168.1.1", Port: 22}}, 300, "must match"},
		{"tcp address is a hostname", []store.CheckTarget{{Name: "web", Address: "web.lan", Port: 443}}, 300, "not an IP literal"},
		{"tcp port zero", []store.CheckTarget{{Name: "web", Address: "192.168.1.1", Port: 0}}, 300, "port 0 out of range"},
		{"tcp port too high", []store.CheckTarget{{Name: "web", Address: "192.168.1.1", Port: 70000}}, 300, "port 70000 out of range"},
		{"zero interval", []store.CheckTarget{{Name: "web", Address: "192.168.1.1", Port: 22}}, 0, "interval_s must be positive"},
		{"negative interval", []store.CheckTarget{{Name: "web", Address: "192.168.1.1", Port: 22}}, -30, "interval_s must be positive"},
		// Kind is read from the stored row and never replaced: an http-shaped
		// target against the stored tcp check must be refused.
		{"kind-changing http target", []store.CheckTarget{{Name: "web", URL: "http://web.lan/health"}}, 300, "not an IP literal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.UpdateCheck(ctx, "at-home", tc.targets, tc.intervalS)
			if err == nil {
				t.Fatalf("UpdateCheck succeeded, want refusal %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if !errors.Is(err, store.ErrValidation) {
				t.Fatalf("err = %v, want it to carry ErrValidation (operator input, not a store failure)", err)
			}
		})
	}

	// A refused update is a no-op on the stored row.
	got, err := st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("get check: %v", err)
	}
	if got.IntervalS != 300 || len(got.Targets) != 2 || got.Targets[1].Address != "192.168.1.60" {
		t.Fatalf("stored check changed after refusals: %+v", got)
	}
}

func TestCheckUpdateNotFound(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	_, err := st.UpdateCheck(ctx, "missing",
		[]store.CheckTarget{{Name: "a", Address: "192.168.1.1", Port: 22}}, 300)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestCheckAggregateCRUD: the opt-in aggregate config round-trips through
// create/get/list, a check without it defaults off, and the reserved aggregate
// state row is excluded from the target count the dashboard card reads.
func TestCheckAggregateCRUD(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: checkFixture().Targets, Aggregate: true, MinUp: 1,
	})
	if err != nil {
		t.Fatalf("create aggregate check: %v", err)
	}
	if !created.Aggregate || created.MinUp != 1 {
		t.Fatalf("created aggregate = %v min_up = %d, want true/1", created.Aggregate, created.MinUp)
	}
	got, err := st.GetCheck(ctx, "home")
	if err != nil {
		t.Fatalf("get aggregate check: %v", err)
	}
	if !got.Aggregate || got.MinUp != 1 {
		t.Fatalf("read aggregate = %v min_up = %d, want true/1", got.Aggregate, got.MinUp)
	}

	plain, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "plain", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("create plain check: %v", err)
	}
	if plain.Aggregate || plain.MinUp != 0 {
		t.Fatalf("plain check aggregate = %v min_up = %d, want off", plain.Aggregate, plain.MinUp)
	}

	now := time.Now().UTC()
	for _, name := range []string{"laptop", "nas", store.AggregateTargetName} {
		if err := st.SetCheckState(ctx, store.CheckState{
			CheckID: created.ID, TargetName: name, Status: store.CheckUp,
			Consecutive: 1, Emitted: store.CheckUp, FirstSeen: now, LastChange: now,
		}); err != nil {
			t.Fatalf("set state %s: %v", name, err)
		}
	}
	// The aggregate row is not a target, so only the two targets count.
	up, err := st.CountCheckStatesByStatus(ctx, store.CheckUp)
	if err != nil {
		t.Fatalf("count up: %v", err)
	}
	if up != 2 {
		t.Fatalf("up targets = %d, want 2 (the reserved aggregate row must not count)", up)
	}
}

// TestCheckAggregateMinUpValidation: min_up is bounded to 0..len(targets) at
// write time, so a threshold no observation could ever satisfy cannot be
// stored.
func TestCheckAggregateMinUpValidation(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		minUp int
	}{
		{"negative", -1},
		{"above target count", 3},
	} {
		_, err := st.CreateCheck(ctx, store.NewCheck{
			Name: "check-" + tc.name, Kind: store.CheckTCP, IntervalS: 300,
			Targets: checkFixture().Targets, Aggregate: true, MinUp: tc.minUp,
		})
		if !errors.Is(err, store.ErrValidation) {
			t.Fatalf("min_up %d: err = %v, want ErrValidation", tc.minUp, err)
		}
	}

	if _, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "check-boundary", Kind: store.CheckTCP, IntervalS: 300,
		Targets: checkFixture().Targets, Aggregate: true, MinUp: 2,
	}); err != nil {
		t.Fatalf("min_up at the target count must be accepted: %v", err)
	}
}

// TestCheckUpdateKeepsAggregateState: a re-point preserves the reserved
// aggregate state row, so editing a check's targets does not silently reset the
// rollup's hysteresis (the reserved name is not a target the prune considers).
func TestCheckUpdateKeepsAggregateState(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: checkFixture().Targets, Aggregate: true, MinUp: 1,
	})
	if err != nil {
		t.Fatalf("create aggregate check: %v", err)
	}
	now := time.Now().UTC()
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: created.ID, TargetName: store.AggregateTargetName, Status: store.CheckUp,
		Consecutive: 2, Emitted: store.CheckDown, FirstSeen: now, LastChange: now,
	}); err != nil {
		t.Fatalf("seed aggregate state: %v", err)
	}

	if _, err := st.UpdateCheck(ctx, "home",
		[]store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}}, 300); err != nil {
		t.Fatalf("update check: %v", err)
	}

	got, err := st.GetCheckState(ctx, created.ID, store.AggregateTargetName)
	if err != nil {
		t.Fatalf("aggregate state was pruned by the update: %v", err)
	}
	if got.Consecutive != 2 || got.Emitted != store.CheckDown {
		t.Fatalf("aggregate state = %+v, want it preserved across the update", got)
	}
}
