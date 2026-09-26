package main

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
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

// runCheckCmdStdin is runCheckCmd with a stdin body, the `-f -` path.
func runCheckCmdStdin(t *testing.T, in string, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(in))
	root.SetArgs(args)

	err := root.Execute()
	return out.String(), err
}

// writeConfig writes a config file for the apply tests.
func writeConfig(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
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

// TestCheckListJSONNetgroupAndRecency: the netgroup the sweep stored on the
// state row and the row's last_change surface in the JSON projection, so a
// remote consumer reads location and recency — not just status — the way the
// checks page does.
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
		Netgroup: "home", LastDurationMS: 21,
	}); err != nil {
		t.Fatalf("set state: %v", err)
	}

	out, err := runCheckCmd(t, "check", "list", "--output", "json")
	if err != nil {
		t.Fatalf("check list json: %v", err)
	}
	for _, want := range []string{`"netgroup":"home"`, `"duration_ms":21`, `"last_change":"2024-05-01T12:00:00Z"`} {
		if !strings.Contains(out, want) {
			t.Errorf("list json missing %q:\n%s", want, out)
		}
	}
}

// TestCheckSnoozeRoundTrip drives the check-level snooze: `snooze --for` sets a
// future expiry and `list` shows it in place of ENABLED, `unsnooze` clears it,
// and a zero/negative --for is refused without touching the row.
func TestCheckSnoozeRoundTrip(t *testing.T) {
	useDataDir(t, t.TempDir())
	ctx := context.Background()

	if _, err := runCheckCmd(t, "check", "add", "at-home",
		"--kind", "tcp", "--interval", "5m", "--target", "laptop=192.168.1.50:22"); err != nil {
		t.Fatalf("check add: %v", err)
	}

	out, err := runCheckCmd(t, "check", "snooze", "at-home", "--for", "4h")
	if err != nil {
		t.Fatalf("check snooze: %v", err)
	}
	if !strings.Contains(out, "snoozed until") {
		t.Errorf("snooze output missing the deadline:\n%s", out)
	}

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	c, err := st.GetCheck(ctx, "at-home")
	_ = st.Close()
	if err != nil {
		t.Fatalf("get check: %v", err)
	}
	if c.SnoozedUntil.IsZero() || !c.SnoozedUntil.After(time.Now().UTC()) {
		t.Fatalf("snoozed_until = %v, want a future expiry", c.SnoozedUntil)
	}

	out, err = runCheckCmd(t, "check", "list")
	if err != nil {
		t.Fatalf("check list: %v", err)
	}
	if !strings.Contains(out, "snoozed") || !strings.Contains(out, "left") {
		t.Errorf("list does not show the snooze:\n%s", out)
	}

	// A zero or negative --for is refused and the snooze is untouched.
	for _, bad := range []string{"0s", "-5m"} {
		if out, err := runCheckCmd(t, "check", "snooze", "at-home", "--for", bad); err == nil {
			t.Fatalf("check snooze --for %s accepted:\n%s", bad, out)
		}
	}
	if rows := checks(t); len(rows) != 1 || rows[0].SnoozedUntil.IsZero() {
		t.Fatalf("a refused snooze changed the row: %+v", rows)
	}

	if _, err := runCheckCmd(t, "check", "unsnooze", "at-home"); err != nil {
		t.Fatalf("check unsnooze: %v", err)
	}
	if rows := checks(t); len(rows) != 1 || !rows[0].SnoozedUntil.IsZero() {
		t.Fatalf("unsnooze left the snooze set: %+v", rows)
	}
	out, err = runCheckCmd(t, "check", "list")
	if err != nil {
		t.Fatalf("check list after unsnooze: %v", err)
	}
	if strings.Contains(out, "snoozed") {
		t.Errorf("list still shows a snooze after unsnooze:\n%s", out)
	}
	if !strings.Contains(out, "enabled") {
		t.Errorf("list does not show enabled after unsnooze:\n%s", out)
	}
}

// TestCheckListShowsNetgroupAndDuration: the text table carries the stored
// netgroup and the last probe duration beside the status, aligned per target,
// so `check list` reads location and latency without --output json.
func TestCheckListShowsNetgroupAndDuration(t *testing.T) {
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
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: c.ID, TargetName: "laptop", Status: store.CheckUp,
		Netgroup: "home", LastDurationMS: 21,
	}); err != nil {
		t.Fatalf("set state: %v", err)
	}

	out, err := runCheckCmd(t, "check", "list")
	if err != nil {
		t.Fatalf("check list: %v", err)
	}
	for _, want := range []string{"NETGROUP", "DURATION", "home", "21ms", "up"} {
		if !strings.Contains(out, want) {
			t.Errorf("list text missing %q:\n%s", want, out)
		}
	}
}

// TestCheckListNetgroupFilterUsesStoredValue: `check list --netgroup` narrows
// the listing to checks with a target whose STORED netgroup matches. The stored
// row deliberately contradicts a live classification of the target's address, so
// a filter that re-classified would land in the wrong group.
func TestCheckListNetgroupFilterUsesStoredValue(t *testing.T) {
	useDataDir(t, t.TempDir())
	ctx := context.Background()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
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
	// The address classifies live as "home"; the sweep's stored verdict says
	// "away", and the filter must honor the stored value.
	if err := st.SetCheckState(ctx, store.CheckState{
		CheckID: c.ID, TargetName: "laptop", Status: store.CheckUp,
		Consecutive: 3, Emitted: store.CheckUp, Netgroup: "away",
	}); err != nil {
		t.Fatalf("set state: %v", err)
	}
	_ = st.Close()

	out, err := runCheckCmd(t, "check", "list", "--netgroup", "away")
	if err != nil {
		t.Fatalf("check list --netgroup away: %v", err)
	}
	if !strings.Contains(out, "at-home") {
		t.Errorf("stored netgroup away did not match the check:\n%s", out)
	}

	out, err = runCheckCmd(t, "check", "list", "--netgroup", "home")
	if err != nil {
		t.Fatalf("check list --netgroup home: %v", err)
	}
	if strings.Contains(out, "at-home") {
		t.Errorf("live netgroup home matched despite the stored away verdict:\n%s", out)
	}

	out, err = runCheckCmd(t, "check", "list", "--netgroup", "nope")
	if err != nil {
		t.Fatalf("check list --netgroup nope: %v", err)
	}
	if strings.Contains(out, "at-home") {
		t.Errorf("unknown netgroup matched the check:\n%s", out)
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

// TestCheckAddKindFlagsRoundTrip drives the dns/tls/metric flag surfaces:
// each kind's config lands on the stored target, and a flag that does not
// belong to the kind (or a metric missing its threshold) is refused without
// writing a row.
func TestCheckAddKindFlagsRoundTrip(t *testing.T) {
	useDataDir(t, t.TempDir())

	if _, err := runCheckCmd(t, "check", "add", "cert",
		"--kind", "tls", "--interval", "1h",
		"--target", "web=web.lan:443", "--min-days", "21"); err != nil {
		t.Fatalf("tls add: %v", err)
	}
	if _, err := runCheckCmd(t, "check", "add", "resolve",
		"--kind", "dns", "--interval", "1h",
		"--target", "api=api.lan", "--expect-addr", "192.168.1.10"); err != nil {
		t.Fatalf("dns add: %v", err)
	}
	if _, err := runCheckCmd(t, "check", "add", "queue",
		"--kind", "metric", "--interval", "1h",
		"--target", "depth=http://nas:9090/queue", "--op", ">", "--value", "100"); err != nil {
		t.Fatalf("metric add: %v", err)
	}

	rows := checksByName(checks(t))
	cert, ok := rows["cert"]
	if !ok || cert.Kind != store.CheckTLS || len(cert.Targets) != 1 ||
		cert.Targets[0].Address != "web.lan" || cert.Targets[0].Port != 443 || cert.Targets[0].MinDays != 21 {
		t.Fatalf("cert = %+v, want a tls target web.lan:443 with min_days 21", cert)
	}
	resolve, ok := rows["resolve"]
	if !ok || resolve.Kind != store.CheckDNS || resolve.Targets[0].DNSName != "api.lan" ||
		resolve.Targets[0].ExpectAddr != "192.168.1.10" {
		t.Fatalf("resolve = %+v, want a dns target api.lan expecting 192.168.1.10", resolve)
	}
	queue, ok := rows["queue"]
	if !ok || queue.Kind != store.CheckMetric || queue.Targets[0].URL != "http://nas:9090/queue" ||
		queue.Targets[0].Op != ">" || queue.Targets[0].Value != 100 {
		t.Fatalf("queue = %+v, want a metric target with op > and value 100", queue)
	}

	for _, args := range [][]string{
		{"check", "add", "x", "--kind", "tcp", "--interval", "1h", "--target", "a=192.168.1.5:22", "--min-days", "21"},
		{"check", "add", "x", "--kind", "tls", "--interval", "1h", "--target", "a=web.lan:443", "--expect-addr", "192.168.1.5"},
		{"check", "add", "x", "--kind", "http", "--interval", "1h", "--target", "a=http://web.lan/x", "--op", ">"},
		{"check", "add", "x", "--kind", "http", "--interval", "1h", "--target", "a=http://web.lan/x", "--value", "1"},
		{"check", "add", "x", "--kind", "metric", "--interval", "1h", "--target", "a=http://web.lan/x", "--value", "1"},
		{"check", "add", "x", "--kind", "metric", "--interval", "1h", "--target", "a=http://web.lan/x", "--op", ">"},
		{"check", "add", "x", "--kind", "metric", "--interval", "1h", "--target", "a=http://web.lan/x", "--op", "~", "--value", "1"},
	} {
		out, err := runCheckCmd(t, args...)
		if err == nil {
			t.Fatalf("check %v accepted, want a refusal:\n%s", args, out)
		}
	}
	if _, ok := checksByName(checks(t))["x"]; ok {
		t.Fatal("a refused add wrote a row")
	}
}

// TestCheckUpdateKindFlags: `check update` re-points a tls or metric check and
// replaces its kind-specific config from the same flags, refusing a flag that
// does not belong to the stored kind.
func TestCheckUpdateKindFlags(t *testing.T) {
	useDataDir(t, t.TempDir())

	if _, err := runCheckCmd(t, "check", "add", "cert",
		"--kind", "tls", "--interval", "1h",
		"--target", "web=web.lan:443", "--min-days", "7"); err != nil {
		t.Fatalf("tls add: %v", err)
	}
	if _, err := runCheckCmd(t, "check", "update", "cert",
		"--target", "web=web.lan:8443", "--min-days", "21"); err != nil {
		t.Fatalf("tls update: %v", err)
	}
	cert := checksByName(checks(t))["cert"]
	if cert.Targets[0].Port != 8443 || cert.Targets[0].MinDays != 21 {
		t.Fatalf("cert after update = %+v, want port 8443 and min_days 21", cert.Targets[0])
	}

	if out, err := runCheckCmd(t, "check", "update", "cert", "--target", "web=web.lan:443", "--expect-addr", "192.168.1.5"); err == nil {
		t.Fatalf("update of a tls check accepted --expect-addr:\n%s", out)
	}
}

// closedLoopback binds a loopback listener, closes it, and returns its address
// and port only once a dial is refused — the deterministic down target for the
// on-demand probe tests, mirroring internal/probe's own helper.
func closedLoopback(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	_ = ln.Close()

	addr := net.JoinHostPort(host, portStr)
	d := net.Dialer{Timeout: 50 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := d.Dial("tcp", addr)
		if derr != nil {
			return host, port
		}
		_ = conn.Close()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("port %s still accepts after the listener closed", addr)
	return "", 0
}

// checkStateCount counts a named check's state rows, the read a probe must not
// change.
func checkStateCount(t *testing.T, name string) int {
	t.Helper()
	ctx := context.Background()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	c, err := st.GetCheck(ctx, name)
	if err != nil {
		t.Fatalf("get check %s: %v", name, err)
	}
	states, err := st.ListCheckStates(ctx, c.ID)
	if err != nil {
		t.Fatalf("list states %s: %v", name, err)
	}
	return len(states)
}

// TestCheckProbeWritesNothing: `check probe` runs each target immediately —
// both loopback targets read down (the live listener refused by the
// address-class guard, the closed port by the dial) — and leaves the store
// exactly as it found it: no state row, no event, no write.
func TestCheckProbeWritesNothing(t *testing.T) {
	useDataDir(t, t.TempDir())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, livePort, _ := net.SplitHostPort(ln.Addr().String())
	closedHost, closedPort := closedLoopback(t)

	if _, err := runCheckCmd(t, "check", "add", "mixed",
		"--kind", "tcp", "--interval", "5m",
		"--target", "live=127.0.0.1:"+livePort,
		"--target", "closed="+net.JoinHostPort(closedHost, strconv.Itoa(closedPort))); err != nil {
		t.Fatalf("check add: %v", err)
	}
	before := checks(t)

	out, err := runCheckCmd(t, "check", "probe", "mixed")
	if err != nil {
		t.Fatalf("check probe: %v", err)
	}
	if !strings.Contains(out, "TARGET") || !strings.Contains(out, "live") || !strings.Contains(out, "DOWN") {
		t.Errorf("probe output missing the refused target result:\n%s", out)
	}
	if !strings.Contains(out, "closed") || !strings.Contains(out, "DOWN") {
		t.Errorf("probe output missing the closed target result:\n%s", out)
	}
	if !strings.Contains(out, "refusing") {
		t.Errorf("probe output missing the guard refusal:\n%s", out)
	}
	if !strings.Contains(out, "away") {
		t.Errorf("probe output missing the live location:\n%s", out)
	}

	after := checks(t)
	if len(before) != len(after) || before[0].UpdatedAt != after[0].UpdatedAt {
		t.Fatalf("check probe changed the check row: before=%+v after=%+v", before, after)
	}
	if n := checkStateCount(t, "mixed"); n != 0 {
		t.Fatalf("check probe wrote %d state row(s)", n)
	}
}

// TestCheckProbeTargetFilter: --target narrows the run to one target and a
// missing name is refused without probing anything.
func TestCheckProbeTargetFilter(t *testing.T) {
	useDataDir(t, t.TempDir())

	host, port := closedLoopback(t)
	if _, err := runCheckCmd(t, "check", "add", "at-home",
		"--kind", "tcp", "--interval", "5m",
		"--target", "laptop="+net.JoinHostPort(host, strconv.Itoa(port))); err != nil {
		t.Fatalf("check add: %v", err)
	}
	out, err := runCheckCmd(t, "check", "probe", "at-home", "--target", "laptop")
	if err != nil {
		t.Fatalf("check probe --target: %v", err)
	}
	if !strings.Contains(out, "laptop") {
		t.Errorf("filtered probe missing the target:\n%s", out)
	}
	if out, err := runCheckCmd(t, "check", "probe", "at-home", "--target", "nope"); err == nil {
		t.Fatalf("probe of a missing target succeeded:\n%s", out)
	}
}

// TestCheckTestWritesNothing: `check test` validates and runs a one-off probe
// set without a store at all — the dry run before `check add` — and a bad
// metric threshold is refused without a row.
func TestCheckTestWritesNothing(t *testing.T) {
	useDataDir(t, t.TempDir())

	host, port := closedLoopback(t)
	out, err := runCheckCmd(t, "check", "test",
		"--kind", "tcp", "--target", "probe="+net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("check test: %v", err)
	}
	if !strings.Contains(out, "probe") || !strings.Contains(out, "DOWN") {
		t.Errorf("check test output missing the down result:\n%s", out)
	}
	if rows := checks(t); len(rows) != 0 {
		t.Fatalf("check test stored a row: %+v", rows)
	}

	if out, err := runCheckCmd(t, "check", "test",
		"--kind", "metric", "--target", "depth=http://nas:9090/queue", "--op", ">"); err == nil {
		t.Fatalf("check test accepted a metric without --value:\n%s", out)
	}
	if rows := checks(t); len(rows) != 0 {
		t.Fatalf("a refused check test stored a row: %+v", rows)
	}
}

// checksByName indexes checks by name for the kind-flag assertions.
func checksByName(rows []store.Check) map[string]store.Check {
	out := make(map[string]store.Check, len(rows))
	for _, c := range rows {
		out[c.Name] = c
	}
	return out
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

// TestCheckExportApplyRoundTrip: export renders every check's declarative form
// (kind config included) deterministically, and feeding that file straight back
// into apply is a no-op — every row unchanged, the second export byte-identical.
func TestCheckExportApplyRoundTrip(t *testing.T) {
	useDataDir(t, t.TempDir())
	ctx := context.Background()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "laptop", Address: "192.168.1.50", Port: 22},
			{Name: "nas", Address: "192.168.1.60", Port: 22},
		},
	}); err != nil {
		t.Fatalf("create tcp check: %v", err)
	}
	if _, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "cert", Kind: store.CheckTLS, IntervalS: 3600,
		Targets: []store.CheckTarget{{Name: "web", Address: "web.lan", Port: 443, MinDays: 21}},
	}); err != nil {
		t.Fatalf("create tls check: %v", err)
	}
	_ = st.Close()

	out, err := runCheckCmd(t, "check", "export")
	if err != nil {
		t.Fatalf("check export: %v", err)
	}
	for _, want := range []string{
		"checks:", "name: at-home", "kind: tcp", "interval: 5m0s", "enabled: true",
		"name: laptop", "address: 192.168.1.50", "port: 22",
		"name: cert", "kind: tls", "interval: 1h0m0s", "min_days: 21",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("export missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "netgroups:") {
		t.Errorf("check export emitted an empty netgroups list:\n%s", out)
	}

	file := writeConfig(t, "checks.yaml", out)
	applyOut, err := runCheckCmd(t, "check", "apply", "-f", file)
	if err != nil {
		t.Fatalf("check apply: %v\n%s", err, applyOut)
	}
	for _, want := range []string{"check at-home: unchanged", "check cert: unchanged", "2 unchanged"} {
		if !strings.Contains(applyOut, want) {
			t.Errorf("apply missing %q:\n%s", want, applyOut)
		}
	}
	if strings.Contains(applyOut, ": updated") || strings.Contains(applyOut, ": created") ||
		strings.Contains(applyOut, ": deleted") {
		t.Errorf("round-trip apply changed a row:\n%s", applyOut)
	}

	again, err := runCheckCmd(t, "check", "export")
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	if again != out {
		t.Errorf("export not stable across a round trip:\n--- first ---\n%s\n--- second ---\n%s", out, again)
	}

	jsonOut, err := runCheckCmd(t, "check", "export", "--output", "json")
	if err != nil {
		t.Fatalf("check export json: %v", err)
	}
	for _, want := range []string{`"checks":`, `"name": "at-home"`, `"interval": "5m0s"`, `"enabled": true`} {
		if !strings.Contains(jsonOut, want) {
			t.Errorf("json export missing %q:\n%s", want, jsonOut)
		}
	}
	jsonFile := writeConfig(t, "checks.json", jsonOut)
	applyJSON, err := runCheckCmd(t, "check", "apply", "-f", jsonFile)
	if err != nil {
		t.Fatalf("apply json: %v\n%s", err, applyJSON)
	}
	if !strings.Contains(applyJSON, "2 unchanged") {
		t.Errorf("json round-trip apply = %q, want all unchanged", applyJSON)
	}
	if _, err := runCheckCmd(t, "check", "export", "--output", "yaml"); err != nil {
		t.Errorf("--output yaml refused: %v", err)
	}
	if out, err := runCheckCmd(t, "check", "export", "--output", "xml"); err == nil {
		t.Errorf("--output xml accepted:\n%s", out)
	}
}

// TestCheckApplyStdin: `-f -` reads the document from stdin, so an export can
// be piped straight into an apply.
func TestCheckApplyStdin(t *testing.T) {
	useDataDir(t, t.TempDir())

	doc := `
checks:
  - name: at-home
    kind: tcp
    interval: 5m
    enabled: true
    targets:
      - name: laptop
        address: 192.168.1.50
        port: 22
`
	out, err := runCheckCmdStdin(t, doc, "check", "apply", "-f", "-")
	if err != nil {
		t.Fatalf("check apply -f -: %v\n%s", err, out)
	}
	if !strings.Contains(out, "check at-home: created") || !strings.Contains(out, "1 created") {
		t.Errorf("stdin apply output = %q, want a created row", out)
	}
	if rows := checks(t); len(rows) != 1 || rows[0].Name != "at-home" {
		t.Fatalf("checks = %+v, want one at-home check", rows)
	}
}

// TestCheckApplyCreateUpdateRefusesKindChange: a new name is created, a changed
// interval updates in place, and a changed kind is refused by name — never
// silently recreated, because that would discard the check's state.
func TestCheckApplyCreateUpdateRefusesKindChange(t *testing.T) {
	useDataDir(t, t.TempDir())

	base := `
checks:
  - name: at-home
    kind: tcp
    interval: %s
    enabled: true
    targets:
      - name: laptop
        address: 192.168.1.50
        port: 22
`
	file := writeConfig(t, "checks.yaml", strings.Replace(base, "%s", "5m", 1))
	out, err := runCheckCmd(t, "check", "apply", "-f", file)
	if err != nil {
		t.Fatalf("apply create: %v\n%s", err, out)
	}
	if !strings.Contains(out, "check at-home: created") {
		t.Errorf("apply create output = %q", out)
	}

	if err := os.WriteFile(file, []byte(strings.Replace(base, "%s", "30s", 1)), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	out, err = runCheckCmd(t, "check", "apply", "-f", file)
	if err != nil {
		t.Fatalf("apply update: %v\n%s", err, out)
	}
	if !strings.Contains(out, "check at-home: updated") {
		t.Errorf("apply update output = %q, want updated", out)
	}
	if rows := checks(t); rows[0].IntervalS != 30 {
		t.Fatalf("interval = %d, want 30 after the update", rows[0].IntervalS)
	}

	kindFile := writeConfig(t, "kind.yaml", `
checks:
  - name: at-home
    kind: http
    interval: 30s
    enabled: true
    targets:
      - name: laptop
        url: http://192.168.1.50/
`)
	out, err = runCheckCmd(t, "check", "apply", "-f", kindFile)
	if err == nil {
		t.Fatalf("kind change accepted:\n%s", out)
	}
	if !strings.Contains(err.Error(), "at-home") || !strings.Contains(err.Error(), "not updatable") {
		t.Errorf("kind refusal = %v, want it to name the check and the rule", err)
	}
	rows := checks(t)
	if len(rows) != 1 || rows[0].Kind != store.CheckTCP || rows[0].Targets[0].Port != 22 {
		t.Fatalf("a refused kind change disturbed the row: %+v", rows)
	}
}

// TestCheckApplyPreflightsEveryRow: every row is fully validated before any
// write, so a file whose last row is invalid leaves the earlier rows untouched
// rather than half applied.
func TestCheckApplyPreflightsEveryRow(t *testing.T) {
	useDataDir(t, t.TempDir())

	file := writeConfig(t, "checks.yaml", `
checks:
  - name: good
    kind: tcp
    interval: 5m
    enabled: true
    targets:
      - name: laptop
        address: 192.168.1.50
        port: 22
  - name: bad
    kind: tcp
    interval: 5m
    enabled: true
    targets:
      - name: nas
        address: 192.168.1.60
        port: 0
`)
	out, err := runCheckCmd(t, "check", "apply", "-f", file)
	if err == nil {
		t.Fatalf("apply accepted a file with an invalid row:\n%s", out)
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("refusal = %v, want it to name the invalid check", err)
	}
	if rows := checks(t); len(rows) != 0 {
		t.Fatalf("a refused apply left %d row(s): %+v", len(rows), rows)
	}
}

// TestCheckApplyPruneNeedsConfirmation: --prune without --yes-really is refused
// by the same confirmation idiom as a global silence, and the estate is
// untouched; with the confirmation the absent row is deleted and the named one
// stays.
func TestCheckApplyPruneNeedsConfirmation(t *testing.T) {
	useDataDir(t, t.TempDir())

	file := writeConfig(t, "checks.yaml", `
checks:
  - name: keep
    kind: tcp
    interval: 5m
    enabled: true
    targets:
      - name: laptop
        address: 192.168.1.50
        port: 22
`)
	if out, err := runCheckCmd(t, "check", "apply", "-f", file); err != nil {
		t.Fatalf("apply seed: %v\n%s", err, out)
	}

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateCheck(context.Background(), store.NewCheck{
		Name: "drop", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{{Name: "nas", Address: "192.168.1.60", Port: 22}},
	}); err != nil {
		t.Fatalf("create drop check: %v", err)
	}
	_ = st.Close()

	out, err := runCheckCmd(t, "check", "apply", "-f", file, "--prune")
	if err == nil {
		t.Fatalf("unconfirmed prune accepted:\n%s", out)
	}
	if !strings.Contains(err.Error(), "yes-really") {
		t.Errorf("prune refusal = %v, want it to name the confirmation flag", err)
	}
	if rows := checks(t); len(rows) != 2 {
		t.Fatalf("unconfirmed prune changed the estate: %+v", rows)
	}

	out, err = runCheckCmd(t, "check", "apply", "-f", file, "--prune", "--yes-really")
	if err != nil {
		t.Fatalf("confirmed prune: %v\n%s", err, out)
	}
	for _, want := range []string{"check drop: deleted", "check keep: unchanged", "1 deleted"} {
		if !strings.Contains(out, want) {
			t.Errorf("prune output missing %q:\n%s", want, out)
		}
	}
	rows := checks(t)
	if len(rows) != 1 || rows[0].Name != "keep" {
		t.Fatalf("prune left %+v, want only keep", rows)
	}
}
