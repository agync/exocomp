package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// runNetgroupCmd executes one `netgroup` invocation against the whole root
// command, so the persistent --output flag resolves exactly as it does for a
// real invocation.
func runNetgroupCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	err := root.Execute()
	return out.String(), err
}

func netgroups(t *testing.T) []store.Netgroup {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	rows, err := st.ListNetgroups(context.Background())
	if err != nil {
		t.Fatalf("list netgroups: %v", err)
	}
	return rows
}

// TestNetgroupAddListRmRoundTrip drives the verbs against a scratch data dir:
// add parses and stores the CIDRs, list renders them (text and JSON), rm
// removes the row, and a second rm reports it gone.
func TestNetgroupAddListRmRoundTrip(t *testing.T) {
	useDataDir(t, t.TempDir())

	if _, err := runNetgroupCmd(t, "netgroup", "add", "home", "192.168.0.0/16,10.0.0.0/8"); err != nil {
		t.Fatalf("netgroup add: %v", err)
	}
	rows := netgroups(t)
	if len(rows) != 1 || rows[0].Name != "home" ||
		len(rows[0].Prefixes) != 2 || rows[0].Prefixes[0].String() != "192.168.0.0/16" {
		t.Fatalf("netgroups = %+v, want one home netgroup with both prefixes", rows)
	}

	out, err := runNetgroupCmd(t, "netgroup", "list")
	if err != nil {
		t.Fatalf("netgroup list: %v", err)
	}
	if !strings.Contains(out, "home") || !strings.Contains(out, "192.168.0.0/16,10.0.0.0/8") {
		t.Errorf("list text = %q, want the group name and both prefixes", out)
	}

	out, err = runNetgroupCmd(t, "netgroup", "list", "--output", "json")
	if err != nil {
		t.Fatalf("netgroup list --output json: %v", err)
	}
	for _, want := range []string{`"kind":"netgroup.list"`, `"name":"home"`,
		`"prefixes":["192.168.0.0/16","10.0.0.0/8"]`} {
		if !strings.Contains(out, want) {
			t.Errorf("list json missing %q:\n%s", want, out)
		}
	}

	if _, err := runNetgroupCmd(t, "netgroup", "rm", "home"); err != nil {
		t.Fatalf("netgroup rm: %v", err)
	}
	if rows := netgroups(t); len(rows) != 0 {
		t.Errorf("netgroups after rm = %+v, want none", rows)
	}

	out, err = runNetgroupCmd(t, "netgroup", "rm", "home")
	if err == nil {
		t.Fatalf("second netgroup rm succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("second rm error = %v, want one naming the miss", err)
	}
}

// TestNetgroupAddRejectsBadInput keeps a row that can never stamp a webhook
// out of the database: an unparseable CIDR and an empty name are refused.
func TestNetgroupAddRejectsBadInput(t *testing.T) {
	useDataDir(t, t.TempDir())

	for _, args := range [][]string{
		{"add", "home", "not-a-cidr"},
		{"add", "home", "192.168.0.0/16,,10.0.0.0/8"},
		{"add", "", "10.0.0.0/8"},
		{"add", "bad name", "10.0.0.0/8"},
	} {
		out, err := runNetgroupCmd(t, args...)
		if err == nil {
			t.Fatalf("netgroup %v accepted, want a refusal:\n%s", args, out)
		}
	}
	if rows := netgroups(t); len(rows) != 0 {
		t.Errorf("refused netgroups were written anyway: %+v", rows)
	}

	if _, err := runNetgroupCmd(t, "netgroup", "add", "home", "192.168.0.0/16"); err != nil {
		t.Fatalf("valid add refused: %v", err)
	}
	out, err := runNetgroupCmd(t, "netgroup", "add", "home", "10.0.0.0/8")
	if err == nil {
		t.Fatalf("duplicate add accepted:\n%s", out)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate error = %v, want one naming the collision", err)
	}
}

// TestNetgroupExclusionsRoundTripThroughVerbs: a `!` list round-trips through
// add and list (text and JSON), show classifies both sides of the exclusion,
// and update replaces the set in place.
func TestNetgroupExclusionsRoundTripThroughVerbs(t *testing.T) {
	useDataDir(t, t.TempDir())

	out, err := runNetgroupCmd(t, "netgroup", "add", "home", "192.168.0.0/16,!192.168.5.0/24")
	if err != nil {
		t.Fatalf("netgroup add: %v\n%s", err, out)
	}
	if !strings.Contains(out, "192.168.0.0/16,!192.168.5.0/24") {
		t.Errorf("add output = %q, want the exclusion echoed", out)
	}
	rows := netgroups(t)
	if len(rows) != 1 || len(rows[0].Prefixes) != 1 || len(rows[0].Excludes) != 1 {
		t.Fatalf("netgroups = %+v, want one include and one exclude", rows)
	}

	out, err = runNetgroupCmd(t, "netgroup", "list")
	if err != nil {
		t.Fatalf("netgroup list: %v", err)
	}
	if !strings.Contains(out, "192.168.0.0/16,!192.168.5.0/24") {
		t.Errorf("list text = %q, want the exclusion rendered", out)
	}

	out, err = runNetgroupCmd(t, "netgroup", "list", "--output", "json")
	if err != nil {
		t.Fatalf("netgroup list json: %v", err)
	}
	for _, want := range []string{`"prefixes":["192.168.0.0/16"]`, `"excludes":["192.168.5.0/24"]`} {
		if !strings.Contains(out, want) {
			t.Errorf("list json missing %q:\n%s", want, out)
		}
	}

	out, err = runNetgroupCmd(t, "netgroup", "show", "192.168.5.10")
	if err != nil {
		t.Fatalf("netgroup show excluded: %v", err)
	}
	if !strings.Contains(out, "away") {
		t.Errorf("show excluded = %q, want away", out)
	}
	out, err = runNetgroupCmd(t, "netgroup", "show", "192.168.1.10")
	if err != nil {
		t.Fatalf("netgroup show included: %v", err)
	}
	for _, want := range []string{"home", "192.168.0.0/16"} {
		if !strings.Contains(out, want) {
			t.Errorf("show included missing %q: %s", want, out)
		}
	}

	out, err = runNetgroupCmd(t, "netgroup", "update", "home", "10.0.0.0/8")
	if err != nil {
		t.Fatalf("netgroup update: %v\n%s", err, out)
	}
	rows = netgroups(t)
	if len(rows) != 1 || len(rows[0].Prefixes) != 1 || rows[0].Prefixes[0].String() != "10.0.0.0/8" || len(rows[0].Excludes) != 0 {
		t.Fatalf("after update = %+v, want only 10.0.0.0/8", rows)
	}
}

// TestNetgroupAddAwayRefused: a group named away is byte-identical to the miss
// label and is refused at write time.
func TestNetgroupAddAwayRefused(t *testing.T) {
	useDataDir(t, t.TempDir())

	out, err := runNetgroupCmd(t, "netgroup", "add", "away", "10.0.0.0/8")
	if err == nil {
		t.Fatalf("netgroup add away accepted:\n%s", out)
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("away refusal = %v, want a reserved-name message", err)
	}
	if rows := netgroups(t); len(rows) != 0 {
		t.Errorf("away netgroup written anyway: %+v", rows)
	}
}

// TestNetgroupUsageReportsObserved: usage reads check_state.netgroup and the
// events' peer_netgroup and renders both per group, filtered on request.
func TestNetgroupUsageReportsObserved(t *testing.T) {
	useDataDir(t, t.TempDir())

	if _, err := runNetgroupCmd(t, "netgroup", "add", "home", "192.168.0.0/16"); err != nil {
		t.Fatalf("netgroup add: %v", err)
	}

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	c, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	now := time.Now().UTC()
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: c.ID, TargetName: "laptop", Status: store.CheckUp, Consecutive: 1,
		Emitted: store.CheckUp, FirstSeen: now, LastChange: now,
		RecentObservations: "[]", Netgroup: "home",
	}); err != nil {
		t.Fatalf("set check state: %v", err)
	}
	if _, err := st.CreateEvent(ctx, store.Event{
		Source:     "generic",
		Outcome:    store.OutcomeIgnored,
		Normalized: `{"labels":{"peer_ip":"192.168.1.9","peer_netgroup":"home"}}`,
	}); err != nil {
		t.Fatalf("create event: %v", err)
	}

	out, err := runNetgroupCmd(t, "netgroup", "usage")
	if err != nil {
		t.Fatalf("netgroup usage: %v\n%s", err, out)
	}
	for _, want := range []string{"NETGROUP", "home", "at-home:laptop", "192.168.1.9"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q:\n%s", want, out)
		}
	}

	out, err = runNetgroupCmd(t, "netgroup", "usage", "home")
	if err != nil {
		t.Fatalf("netgroup usage home: %v\n%s", err, out)
	}
	if !strings.Contains(out, "at-home:laptop") || strings.Contains(out, "away") {
		t.Errorf("filtered usage = %q, want only home", out)
	}
}
