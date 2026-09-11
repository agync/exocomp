package main

import (
	"bytes"
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// runCheckCmd executes one `check` invocation against the whole root command,
// so the persistent --output flag resolves exactly as it does for a real
// invocation.
func runCheckCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	err := root.Execute()
	return out.String(), err
}

func checks(t *testing.T) []store.Check {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	rows, err := st.ListChecks(context.Background(), false)
	if err != nil {
		t.Fatalf("list checks: %v", err)
	}
	return rows
}

// TestCheckAddListEnableDisableRmRoundTrip drives the verbs against a scratch
// data dir: add stores the probe targets, list renders them (text and JSON),
// enable/disable flip the sweep's view, rm removes the row, and a second rm
// reports it gone.
func TestCheckAddListEnableDisableRmRoundTrip(t *testing.T) {
	useDataDir(t, t.TempDir())

	if _, err := runCheckCmd(t, "check", "add", "at-home",
		"--kind", "tcp", "--interval", "5m",
		"--target", "laptop=192.168.1.50:22", "--target", "nas=192.168.1.60:22"); err != nil {
		t.Fatalf("check add: %v", err)
	}
	rows := checks(t)
	if len(rows) != 1 || rows[0].Name != "at-home" || rows[0].Kind != store.CheckTCP ||
		rows[0].IntervalS != 300 || len(rows[0].Targets) != 2 ||
		rows[0].Targets[1].Address != "192.168.1.60" || rows[0].Targets[1].Port != 22 {
		t.Fatalf("checks = %+v, want one tcp at-home check every 300s with both targets", rows)
	}

	out, err := runCheckCmd(t, "check", "list")
	if err != nil {
		t.Fatalf("check list: %v", err)
	}
	for _, want := range []string{"NAME", "STATUS", "at-home", "tcp", "5m0s", "laptop,nas",
		"unobserved,unobserved", "enabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("list text missing %q:\n%s", want, out)
		}
	}

	out, err = runCheckCmd(t, "check", "list", "--output", "json")
	if err != nil {
		t.Fatalf("check list --output json: %v", err)
	}
	for _, want := range []string{`"kind":"check.list"`, `"name":"at-home"`,
		`"interval_s":300`, `"enabled":true`, `"address":"192.168.1.50"`, `"port":22`} {
		if !strings.Contains(out, want) {
			t.Errorf("list json missing %q:\n%s", want, out)
		}
	}

	if _, err := runCheckCmd(t, "check", "disable", "at-home"); err != nil {
		t.Fatalf("check disable: %v", err)
	}
	if rows := checks(t); len(rows) != 1 || rows[0].Enabled {
		t.Fatalf("after disable: %+v, want the check present but disabled", rows)
	}
	if _, err := runCheckCmd(t, "check", "enable", "at-home"); err != nil {
		t.Fatalf("check enable: %v", err)
	}
	if rows := checks(t); len(rows) != 1 || !rows[0].Enabled {
		t.Fatalf("after enable: %+v, want the check enabled again", rows)
	}

	if _, err := runCheckCmd(t, "check", "rm", "at-home"); err != nil {
		t.Fatalf("check rm: %v", err)
	}
	if rows := checks(t); len(rows) != 0 {
		t.Fatalf("checks after rm = %+v, want none", rows)
	}

	out, err = runCheckCmd(t, "check", "rm", "at-home")
	if err == nil {
		t.Fatalf("second check rm succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("second rm error = %v, want one naming the miss", err)
	}
}

// TestCheckListJSONNetgroupAndRecency: a registered netgroup covering a target
// and a seeded state row surface in the JSON projection as netgroup and
// last_change, so a remote consumer reads location and recency — not just
// status — the way the checks page does.
func TestCheckListJSONNetgroupAndRecency(t *testing.T) {
	useDataDir(t, t.TempDir())
	ctx := context.Background()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.CreateNetgroup(ctx, store.NewNetgroup{
		Name:     "home",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")},
	}); err != nil {
		t.Fatalf("create netgroup: %v", err)
	}
	c, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	lastChange := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: c.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 3, Emitted: store.CheckUp,
		FirstSeen: lastChange.Add(-time.Hour), LastChange: lastChange,
	}); err != nil {
		t.Fatalf("set state: %v", err)
	}

	out, err := runCheckCmd(t, "check", "list", "--output", "json")
	if err != nil {
		t.Fatalf("check list json: %v", err)
	}
	for _, want := range []string{`"netgroup":"home"`, `"last_change":"2024-05-01T12:00:00Z"`} {
		if !strings.Contains(out, want) {
			t.Errorf("list json missing %q:\n%s", want, out)
		}
	}
}

// TestCheckAddRejectsBadInput keeps a row that can never probe out of the
// database: a bad kind, a non-positive interval, malformed targets, and
// --expect on a non-http kind are all refused.
func TestCheckAddRejectsBadInput(t *testing.T) {
	useDataDir(t, t.TempDir())

	for _, args := range [][]string{
		{"check", "add", "x", "--kind", "snmp", "--interval", "5m", "--target", "a=127.0.0.1:22"},
		{"check", "add", "x", "--kind", "tcp", "--interval", "0s", "--target", "a=127.0.0.1:22"},
		{"check", "add", "x", "--kind", "tcp", "--interval", "-5m", "--target", "a=127.0.0.1:22"},
		{"check", "add", "x", "--kind", "tcp", "--interval", "5m", "--target", "no-equals"},
		{"check", "add", "x", "--kind", "tcp", "--interval", "5m", "--target", "a=127.0.0.1"},       // tcp needs a port
		{"check", "add", "x", "--kind", "tcp", "--interval", "5m", "--target", "a=127.0.0.1:70000"}, // port out of range
		{"check", "add", "x", "--kind", "http", "--interval", "5m", "--target", "a=not a url"},      // unparseable
		{"check", "add", "x", "--kind", "tcp", "--interval", "5m", "--target", "a=127.0.0.1:22", "--expect", "200"},
		{"check", "add", "x", "--kind", "http", "--interval", "5m", "--target", "a=http://web.lan/x", "--expect", "42"}, // expect out of range
		{"check", "add", "x", "--kind", "ping", "--interval", "5m", "--target", "a=127.0.0.1:22"},                       // ping carries a port
	} {
		out, err := runCheckCmd(t, args...)
		if err == nil {
			t.Fatalf("check %v accepted, want a refusal:\n%s", args, out)
		}
	}
	if rows := checks(t); len(rows) != 0 {
		t.Errorf("refused checks were written anyway: %+v", rows)
	}

	// A duplicate name is refused with the collision named.
	if _, err := runCheckCmd(t, "check", "add", "dup",
		"--kind", "tcp", "--interval", "5m", "--target", "a=127.0.0.1:22"); err != nil {
		t.Fatalf("valid add refused: %v", err)
	}
	out, err := runCheckCmd(t, "check", "add", "dup",
		"--kind", "tcp", "--interval", "5m", "--target", "a=127.0.0.1:23")
	if err == nil {
		t.Fatalf("duplicate add accepted:\n%s", out)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate error = %v, want one naming the collision", err)
	}
}

// TestCheckAddPingRefusedWhenBinaryMissing: a ping check on a host without the
// binary on PATH is refused, because a check that is silently all-down is the
// observability failure the capability probe exists to prevent.
func TestCheckAddPingRefusedWhenBinaryMissing(t *testing.T) {
	useDataDir(t, t.TempDir())
	t.Setenv("PATH", t.TempDir())

	out, err := runCheckCmd(t, "check", "add", "gate",
		"--kind", "ping", "--interval", "5m", "--target", "g=127.0.0.1")
	if err == nil {
		t.Fatalf("ping check accepted on a host without the binary:\n%s", out)
	}
	if !strings.Contains(err.Error(), "ping binary not on PATH") {
		t.Errorf("refusal = %v, want it to name the missing binary", err)
	}
	if rows := checks(t); len(rows) != 0 {
		t.Errorf("refused ping check was written anyway: %+v", rows)
	}
}

// TestCheckUpdateRoundTrip: `check update` re-points a stored check's targets
// and interval in place, printing the new target count and interval, keeping
// the kind. A re-point with the target NAME kept is what lets the sweep flip
// the preserved state row and emit a real transition.
func TestCheckUpdateRoundTrip(t *testing.T) {
	useDataDir(t, t.TempDir())

	if _, err := runCheckCmd(t, "check", "add", "at-home",
		"--kind", "tcp", "--interval", "5m", "--target", "laptop=192.168.1.50:22"); err != nil {
		t.Fatalf("check add: %v", err)
	}

	out, err := runCheckCmd(t, "check", "update", "at-home",
		"--target", "laptop=127.0.0.1:2222", "--interval", "30s")
	if err != nil {
		t.Fatalf("check update: %v", err)
	}
	for _, want := range []string{"updated", "1 target", "30s"} {
		if !strings.Contains(out, want) {
			t.Errorf("update output missing %q:\n%s", want, out)
		}
	}

	rows := checks(t)
	if len(rows) != 1 || rows[0].Kind != store.CheckTCP || rows[0].IntervalS != 30 ||
		len(rows[0].Targets) != 1 || rows[0].Targets[0].Address != "127.0.0.1" || rows[0].Targets[0].Port != 2222 {
		t.Fatalf("checks = %+v, want one tcp check re-pointed to 127.0.0.1:2222", rows)
	}

	// --interval omitted keeps the stored cadence (30s from the update above).
	if _, err := runCheckCmd(t, "check", "update", "at-home",
		"--target", "nas=192.168.1.60:22"); err != nil {
		t.Fatalf("check update without --interval: %v", err)
	}
	if rows := checks(t); len(rows) != 1 || rows[0].IntervalS != 30 ||
		len(rows[0].Targets) != 1 || rows[0].Targets[0].Name != "nas" {
		t.Fatalf("checks after interval-kept update = %+v", rows)
	}
}

// TestCheckUpdateRejectsBadInput: a refused update must neither change the
// stored row nor write a row that can never probe — junk targets, a --kind
// flag (kind is not updatable), a bad interval, and a missing check are all
// refused loudly.
func TestCheckUpdateRejectsBadInput(t *testing.T) {
	useDataDir(t, t.TempDir())

	if _, err := runCheckCmd(t, "check", "add", "at-home",
		"--kind", "tcp", "--interval", "5m", "--target", "a=127.0.0.1:22"); err != nil {
		t.Fatalf("check add: %v", err)
	}

	for _, args := range [][]string{
		{"check", "update", "at-home", "--target", "no-equals"},
		{"check", "update", "at-home", "--target", "a=127.0.0.1:70000"},
		{"check", "update", "at-home", "--kind", "tcp", "--target", "a=127.0.0.1:22"},
	} {
		out, err := runCheckCmd(t, args...)
		if err == nil {
			t.Fatalf("check %v accepted, want a refusal:\n%s", args, out)
		}
	}

	// The stored row is untouched after the refusals.
	rows := checks(t)
	if len(rows) != 1 || rows[0].IntervalS != 300 || len(rows[0].Targets) != 1 || rows[0].Targets[0].Port != 22 {
		t.Fatalf("checks after refusals = %+v, want the original untouched", rows)
	}

	// A missing check reports not-found.
	out, err := runCheckCmd(t, "check", "update", "nope",
		"--target", "a=127.0.0.1:22", "--interval", "5m")
	if err == nil {
		t.Fatalf("update of a missing check succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing error = %v, want it to name the miss", err)
	}
}
