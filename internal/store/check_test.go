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
