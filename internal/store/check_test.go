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
