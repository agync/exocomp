package mcp_test

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/mcp"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// tcpCheck registers a tcp check the store accepts without any network: tcp
// targets are validated as IP literals at write time and only dialed when a
// probe runs.
func tcpCheck(t *testing.T, st *store.Store, name string, targets ...store.CheckTarget) store.Check {
	t.Helper()
	c, err := st.CreateCheck(context.Background(), store.NewCheck{
		Name: name, Kind: store.CheckTCP, IntervalS: 300, Targets: targets,
	})
	if err != nil {
		t.Fatalf("CreateCheck(%s): %v", name, err)
	}
	return c
}

func ipTarget(name, addr string, port int) store.CheckTarget {
	return store.CheckTarget{Name: name, Address: addr, Port: port}
}

// TestCheckToolsCatalogScoped asserts the new checks/netgroups tools obey the
// capability floors: a readonly token sees the read tools (probe included) but
// no write tool; an operator token sees the write tools.
func TestCheckToolsCatalogScoped(t *testing.T) {
	srv, _ := newTestServer(t)
	line := req(1, "tools/list", "{}")

	ro := toolNames(t, call(t, srv, mcp.TokenCaller(store.ScopeReadonly, "ro", 1, nil), line).Result)
	for _, want := range []string{"list_checks", "get_check", "list_netgroups", "probe_check"} {
		if !ro[want] {
			t.Errorf("readonly catalog is missing read tool %q", want)
		}
	}
	writes := []string{
		"check_add", "check_update", "check_enable", "check_disable", "check_rm",
		"check_snooze", "check_unsnooze", "netgroup_add", "netgroup_update", "netgroup_rm",
	}
	for _, w := range writes {
		if ro[w] {
			t.Errorf("readonly token is advertised the write tool %q", w)
		}
	}

	op := toolNames(t, call(t, srv, mcp.TokenCaller(store.ScopeOperator, "op", 2, nil), line).Result)
	for _, w := range writes {
		if !op[w] {
			t.Errorf("operator catalog is missing write tool %q", w)
		}
	}
}

// TestCheckReadToolsProjectWire asserts list_checks and get_check emit the same
// wire kinds and shape as the CLI/API: the stored target config plus the last
// status, netgroup, and duration from check_state.
func TestCheckReadToolsProjectWire(t *testing.T) {
	srv, st := newTestServer(t)
	c := tcpCheck(t, st, "at-home", ipTarget("laptop", "192.168.1.50", 22))

	resp := call(t, srv, driveCaller(), req(1, "tools/call", `{"name":"list_checks","arguments":{}}`))
	env, isErr := toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("list_checks errored: %s", resp.Result)
	}
	if env.Kind != "check.list" {
		t.Fatalf("list_checks kind = %q, want check.list", env.Kind)
	}
	var list []wire.CheckView
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("decode check.list: %v", err)
	}
	if len(list) != 1 || list[0].Name != "at-home" || list[0].Kind != store.CheckTCP {
		t.Fatalf("checks = %+v", list)
	}
	if len(list[0].Targets) != 1 || list[0].Targets[0].Address != "192.168.1.50" {
		t.Fatalf("target config = %+v", list[0].Targets)
	}

	resp = call(t, srv, driveCaller(), req(2, "tools/call", `{"name":"get_check","arguments":{"name":"at-home"}}`))
	env, isErr = toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("get_check errored: %s", resp.Result)
	}
	if env.Kind != "check.get" {
		t.Fatalf("get_check kind = %q, want check.get", env.Kind)
	}
	var got wire.CheckView
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("decode check.get: %v", err)
	}
	if got.ID != c.ID || got.IntervalS != 300 {
		t.Fatalf("get_check view = %+v (want id %d interval 300)", got, c.ID)
	}

	resp = call(t, srv, driveCaller(), req(3, "tools/call", `{"name":"get_check","arguments":{"name":"ghost"}}`))
	env, isErr = toolContent(t, resp.Result)
	if !isErr || env.Error == nil || env.Error.Code != "not_found" {
		t.Fatalf("unknown get_check = %+v, want not_found", env.Error)
	}
}

// TestNetgroupReadTool asserts list_netgroups projects the CIDR set with
// exclusions through the shared netgroup.list kind.
func TestNetgroupReadTool(t *testing.T) {
	srv, st := newTestServer(t)
	_, err := st.CreateNetgroup(context.Background(), store.NewNetgroup{
		Name:     "home",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")},
		Excludes: []netip.Prefix{netip.MustParsePrefix("192.168.5.0/24")},
	})
	if err != nil {
		t.Fatalf("CreateNetgroup: %v", err)
	}

	resp := call(t, srv, driveCaller(), req(1, "tools/call", `{"name":"list_netgroups","arguments":{}}`))
	env, isErr := toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("list_netgroups errored: %s", resp.Result)
	}
	if env.Kind != "netgroup.list" {
		t.Fatalf("kind = %q, want netgroup.list", env.Kind)
	}
	var rows []wire.NetgroupView
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatalf("decode netgroup.list: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "home" ||
		len(rows[0].Prefixes) != 1 || rows[0].Prefixes[0] != "192.168.0.0/16" ||
		len(rows[0].Excludes) != 1 || rows[0].Excludes[0] != "192.168.5.0/24" {
		t.Fatalf("netgroups = %+v", rows)
	}
}

// TestProbeCheckWritesNothing is D3's guard: probe_check answers with a real
// per-target verdict and provably writes nothing — no check_state row moves, no
// event is recorded, and the check row is untouched. A readonly token may call
// it: it is a read-scope operation. The runner uses the sweep's options
// (AllowLoopback=false), so both loopback targets read down — the live one with
// the guard's refusal, the closed one from the dial.
func TestProbeCheckWritesNothing(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	// One live loopback listener (refused by the address-class guard) and one
	// closed loopback port (refused by the dial).
	c := tcpCheck(t, st, "at-home",
		ipTarget("live", "127.0.0.1", port),
		ipTarget("closed", "127.0.0.1", 1),
	)
	before, err := st.GetCheck(ctx, c.Name)
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	eventsBefore, err := st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	resp := call(t, srv, mcp.TokenCaller(store.ScopeReadonly, "ro", 1, nil),
		req(1, "tools/call", `{"name":"probe_check","arguments":{"name":"at-home"}}`))
	env, isErr := toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("probe_check errored: %s", resp.Result)
	}
	if env.Kind != "check.probe" {
		t.Fatalf("kind = %q, want check.probe", env.Kind)
	}
	var view wire.CheckProbeView
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("decode check.probe: %v", err)
	}
	if view.Check != "at-home" || len(view.Targets) != 2 {
		t.Fatalf("probe view = %+v", view)
	}
	byName := map[string]wire.CheckProbeTargetView{}
	for _, tgt := range view.Targets {
		byName[tgt.Target] = tgt
	}
	if byName["live"].Up {
		t.Errorf("live loopback target reads up: %+v", byName["live"])
	}
	if !strings.Contains(byName["live"].Detail, "refusing") {
		t.Errorf("live loopback target detail = %q, want the refusal", byName["live"].Detail)
	}
	if byName["closed"].Up {
		t.Errorf("closed target reads up: %+v", byName["closed"])
	}
	if byName["closed"].Detail == "" {
		t.Errorf("closed target has no detail sentence: %+v", byName["closed"])
	}

	states, err := st.ListCheckStates(ctx, c.ID)
	if err != nil {
		t.Fatalf("ListCheckStates: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("probe_check wrote %d check_state rows", len(states))
	}
	eventsAfter, err := st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("probe_check emitted an event: %d -> %d", len(eventsBefore), len(eventsAfter))
	}
	after, err := st.GetCheck(ctx, c.Name)
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("probe_check touched the check row: %s -> %s", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestProbeCheckNarrowsTarget asserts the optional target argument narrows the
// run, and an unknown target is a boundary error.
func TestProbeCheckNarrowsTarget(t *testing.T) {
	srv, st := newTestServer(t)
	tcpCheck(t, st, "at-home", ipTarget("laptop", "127.0.0.1", 1), ipTarget("nas", "127.0.0.1", 2))

	resp := call(t, srv, driveCaller(),
		req(1, "tools/call", `{"name":"probe_check","arguments":{"name":"at-home","target":"nas"}}`))
	env, isErr := toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("probe_check errored: %s", resp.Result)
	}
	var view wire.CheckProbeView
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Targets) != 1 || view.Targets[0].Target != "nas" {
		t.Fatalf("narrowed targets = %+v", view.Targets)
	}

	resp = call(t, srv, driveCaller(),
		req(2, "tools/call", `{"name":"probe_check","arguments":{"name":"at-home","target":"ghost"}}`))
	env, isErr = toolContent(t, resp.Result)
	if !isErr || env.Error == nil || env.Error.Code != "invalid_argument" {
		t.Fatalf("unknown target = %+v, want invalid_argument", env.Error)
	}
}

// TestCheckWriteToolsRoundTrip drives the check write tools through the same
// store verbs the CLI uses and proves the store changed, not just the result.
func TestCheckWriteToolsRoundTrip(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	// a readonly token is refused before the store is touched.
	resp := call(t, srv, mcp.TokenCaller(store.ScopeReadonly, "ro", 1, nil),
		req(1, "tools/call", `{"name":"check_add","arguments":{"name":"at-home","kind":"tcp","interval":"5m","targets":["laptop=192.168.1.50:22"]}}`))
	env, isErr := toolContent(t, resp.Result)
	if !isErr || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("readonly check_add = %+v, want forbidden", env.Error)
	}
	if _, err := st.GetCheck(ctx, "at-home"); err == nil {
		t.Fatal("a refused check_add still created the row")
	}

	// operator: add.
	resp = call(t, srv, driveCaller(),
		req(2, "tools/call", `{"name":"check_add","arguments":{"name":"at-home","kind":"tcp","interval":"5m","targets":["laptop=192.168.1.50:22"]}}`))
	env, isErr = toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("check_add errored: %s", resp.Result)
	}
	if env.Kind != "check.get" {
		t.Fatalf("check_add kind = %q, want check.get", env.Kind)
	}
	created, err := st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("check_add did not persist: %v", err)
	}
	if created.IntervalS != 300 || len(created.Targets) != 1 {
		t.Fatalf("created check = %+v", created)
	}

	// update re-points the target and keeps the name's state (none here).
	resp = call(t, srv, driveCaller(),
		req(3, "tools/call", `{"name":"check_update","arguments":{"name":"at-home","interval":"10m","targets":["laptop=192.168.1.51:22"]}}`))
	env, isErr = toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("check_update errored: %s", resp.Result)
	}
	updated, err := st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck after update: %v", err)
	}
	if updated.IntervalS != 600 || updated.Targets[0].Address != "192.168.1.51" {
		t.Fatalf("updated check = %+v", updated)
	}

	// disable then enable moves the stored flag.
	for _, tc := range []struct {
		tool    string
		enabled bool
	}{
		{"check_disable", false},
		{"check_enable", true},
	} {
		resp = call(t, srv, driveCaller(),
			req(4, "tools/call", `{"name":"`+tc.tool+`","arguments":{"name":"at-home"}}`))
		if _, isErr = toolContent(t, resp.Result); isErr {
			t.Fatalf("%s errored: %s", tc.tool, resp.Result)
		}
		got, err := st.GetCheck(ctx, "at-home")
		if err != nil {
			t.Fatalf("GetCheck: %v", err)
		}
		if got.Enabled != tc.enabled {
			t.Fatalf("%s left enabled=%v", tc.tool, got.Enabled)
		}
	}

	// snooze sets a future deadline; unsnooze clears it.
	resp = call(t, srv, driveCaller(),
		req(5, "tools/call", `{"name":"check_snooze","arguments":{"name":"at-home","for":"4h"}}`))
	if _, isErr = toolContent(t, resp.Result); isErr {
		t.Fatalf("check_snooze errored: %s", resp.Result)
	}
	snoozed, err := st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if !snoozed.SnoozedUntil.After(time.Now()) {
		t.Fatalf("snoozed_until = %s, want a future time", snoozed.SnoozedUntil)
	}
	resp = call(t, srv, driveCaller(),
		req(6, "tools/call", `{"name":"check_unsnooze","arguments":{"name":"at-home"}}`))
	if _, isErr = toolContent(t, resp.Result); isErr {
		t.Fatalf("check_unsnooze errored: %s", resp.Result)
	}
	cleared, err := st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if !cleared.SnoozedUntil.IsZero() {
		t.Fatalf("unsnooze left snoozed_until = %s", cleared.SnoozedUntil)
	}

	// rm removes the row.
	resp = call(t, srv, driveCaller(),
		req(7, "tools/call", `{"name":"check_rm","arguments":{"name":"at-home"}}`))
	env, isErr = toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("check_rm errored: %s", resp.Result)
	}
	if env.Kind != "check.deleted" {
		t.Fatalf("check_rm kind = %q, want check.deleted", env.Kind)
	}
	if _, err := st.GetCheck(ctx, "at-home"); err == nil {
		t.Fatal("check_rm left the row in the store")
	}
}

// TestNetgroupWriteToolsRoundTrip drives netgroup add/update/rm through the
// store and proves the CIDR set moves.
func TestNetgroupWriteToolsRoundTrip(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	resp := call(t, srv, mcp.TokenCaller(store.ScopeReadonly, "ro", 1, nil),
		req(1, "tools/call", `{"name":"netgroup_add","arguments":{"name":"home","prefixes":"192.168.0.0/16"}}`))
	env, isErr := toolContent(t, resp.Result)
	if !isErr || env.Error == nil || env.Error.Code != "forbidden" {
		t.Fatalf("readonly netgroup_add = %+v, want forbidden", env.Error)
	}

	resp = call(t, srv, driveCaller(),
		req(2, "tools/call", `{"name":"netgroup_add","arguments":{"name":"home","prefixes":"192.168.0.0/16,!192.168.5.0/24"}}`))
	env, isErr = toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("netgroup_add errored: %s", resp.Result)
	}
	if env.Kind != "netgroup.get" {
		t.Fatalf("netgroup_add kind = %q, want netgroup.get", env.Kind)
	}
	g, err := st.GetNetgroup(ctx, "home")
	if err != nil {
		t.Fatalf("netgroup_add did not persist: %v", err)
	}
	if len(g.Prefixes) != 1 || len(g.Excludes) != 1 {
		t.Fatalf("created netgroup = %+v", g)
	}

	resp = call(t, srv, driveCaller(),
		req(3, "tools/call", `{"name":"netgroup_update","arguments":{"name":"home","prefixes":"10.0.0.0/8"}}`))
	if _, isErr = toolContent(t, resp.Result); isErr {
		t.Fatalf("netgroup_update errored: %s", resp.Result)
	}
	g, err = st.GetNetgroup(ctx, "home")
	if err != nil {
		t.Fatalf("GetNetgroup: %v", err)
	}
	if len(g.Prefixes) != 1 || g.Prefixes[0].String() != "10.0.0.0/8" || len(g.Excludes) != 0 {
		t.Fatalf("updated netgroup = %+v", g)
	}

	resp = call(t, srv, driveCaller(),
		req(4, "tools/call", `{"name":"netgroup_rm","arguments":{"name":"home"}}`))
	env, isErr = toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("netgroup_rm errored: %s", resp.Result)
	}
	if env.Kind != "netgroup.deleted" {
		t.Fatalf("netgroup_rm kind = %q, want netgroup.deleted", env.Kind)
	}
	if _, err := st.GetNetgroup(ctx, "home"); err == nil {
		t.Fatal("netgroup_rm left the row in the store")
	}
}

// TestCheckWriteBadArgs asserts bad input is a boundary invalid_argument, not a
// silently-empty mutation or a 500.
func TestCheckWriteBadArgs(t *testing.T) {
	srv, _ := newTestServer(t)
	cases := map[string]string{
		"missing kind":     `{"name":"x","interval":"5m","targets":["a=192.168.1.1:22"]}`,
		"bad interval":     `{"name":"x","kind":"tcp","interval":"soon","targets":["a=192.168.1.1:22"]}`,
		"bad target spec":  `{"name":"x","kind":"tcp","interval":"5m","targets":["a"]}`,
		"tls no host":      `{"name":"x","kind":"tls","interval":"5m","targets":["x=:443"]}`,
		"no targets":       `{"name":"x","kind":"tcp","interval":"5m","targets":[]}`,
		"wrong-kind field": `{"name":"x","kind":"tcp","interval":"5m","targets":["a=192.168.1.1:22"],"min_days":21}`,
		"metric no op":     `{"name":"x","kind":"metric","interval":"5m","targets":["a=http://example.test"]}`,
	}
	for name, body := range cases {
		resp := call(t, srv, driveCaller(), req(1, "tools/call", `{"name":"check_add","arguments":`+body+`}`))
		env, isErr := toolContent(t, resp.Result)
		if !isErr || env.Error == nil || env.Error.Code != "invalid_argument" {
			t.Errorf("%s: %+v, want invalid_argument", name, env.Error)
		}
	}

	// netgroup with no include is a boundary error too.
	resp := call(t, srv, driveCaller(),
		req(2, "tools/call", `{"name":"netgroup_add","arguments":{"name":"home","prefixes":""}}`))
	env, isErr := toolContent(t, resp.Result)
	if !isErr || env.Error == nil || env.Error.Code != "invalid_argument" {
		t.Fatalf("empty netgroup prefixes = %+v, want invalid_argument", env.Error)
	}
}

// TestCheckAggregateToolRoundTrip: check_add accepts the rollup args and
// check_update changes them, and neither the get view nor list_checks presents
// the reserved "*" rollup row as a target.
func TestCheckAggregateToolRoundTrip(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	resp := call(t, srv, driveCaller(),
		req(1, "tools/call", `{"name":"check_add","arguments":{"name":"at-home","kind":"tcp","interval":"5m","targets":["laptop=192.168.1.50:22","nas=192.168.1.60:22"],"aggregate":true,"min_up":1}}`))
	env, isErr := toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("check_add errored: %s", resp.Result)
	}
	var view wire.CheckView
	if err := json.Unmarshal(env.Data, &view); err != nil {
		t.Fatalf("decode check.get: %v", err)
	}
	if !view.Aggregate || view.MinUp != 1 {
		t.Fatalf("check_add view aggregate = %v min_up = %d, want true/1", view.Aggregate, view.MinUp)
	}
	c, err := st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if !c.Aggregate || c.MinUp != 1 {
		t.Fatalf("stored aggregate = %v min_up = %d, want true/1", c.Aggregate, c.MinUp)
	}
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: c.ID, TargetName: store.AggregateTargetName, Status: store.CheckUp,
	}); err != nil {
		t.Fatalf("SetCheckState aggregate: %v", err)
	}

	resp = call(t, srv, driveCaller(),
		req(2, "tools/call", `{"name":"list_checks","arguments":{}}`))
	env, isErr = toolContent(t, resp.Result)
	if isErr {
		t.Fatalf("list_checks errored: %s", resp.Result)
	}
	var list []wire.CheckView
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("decode check.list: %v", err)
	}
	for _, tgt := range list[0].Targets {
		if tgt.Name == store.AggregateTargetName {
			t.Fatalf("list_checks presented the aggregate row as a target: %+v", list[0].Targets)
		}
	}

	resp = call(t, srv, driveCaller(),
		req(3, "tools/call", `{"name":"check_update","arguments":{"name":"at-home","targets":["laptop=192.168.1.50:22","nas=192.168.1.60:22"],"aggregate":false}}`))
	if _, isErr = toolContent(t, resp.Result); isErr {
		t.Fatalf("check_update errored: %s", resp.Result)
	}
	c, err = st.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck after update: %v", err)
	}
	if c.Aggregate || c.MinUp != 0 {
		t.Fatalf("after update aggregate = %v min_up = %d, want false/0", c.Aggregate, c.MinUp)
	}
}
