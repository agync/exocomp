package executor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

// TestParseRecapHostsCounts pulls unreachable and failed counts out of a recap,
// beside the changed count the drift twin already reads.
func TestParseRecapHostsCounts(t *testing.T) {
	t.Parallel()
	lines := []string{
		"PLAY RECAP ****",
		"web1  : ok=5 changed=2 unreachable=0 failed=0 skipped=1 rescued=0 ignored=0",
		"lap   : ok=0 changed=0 unreachable=1 failed=0 skipped=0 rescued=0 ignored=0",
		"db    : ok=3 changed=0 unreachable=0 failed=1 skipped=0 rescued=0 ignored=0",
	}
	hosts, found := executor.ParseRecapHosts(lines)
	if !found || len(hosts) != 3 {
		t.Fatalf("hosts=%+v found=%v, want 3 rows found", hosts, found)
	}
	byName := map[string]executor.RecapHost{}
	for _, h := range hosts {
		byName[h.Host] = h
	}
	if byName["web1"].Changed != 2 {
		t.Errorf("web1 changed=%d, want 2", byName["web1"].Changed)
	}
	if byName["lap"].Unreachable != 1 {
		t.Errorf("lap unreachable=%d, want 1", byName["lap"].Unreachable)
	}
	if byName["db"].Failed != 1 {
		t.Errorf("db failed=%d, want 1", byName["db"].Failed)
	}
}

// TestClassifyOutcome covers both record forms: a playbook's PLAY RECAP and an
// ad-hoc run's host|STATUS lines, plus the fail-closed no-record case.
func TestClassifyOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		lines           []string
		wantFound       bool
		wantUnreachable []string
		wantFailed      []string
	}{
		{
			name: "recap unreachable only",
			lines: []string{
				"PLAY RECAP ***",
				"localhost : ok=1 changed=0 unreachable=0 failed=0 skipped=0 rescued=0 ignored=0",
				"laptop    : ok=0 changed=0 unreachable=1 failed=0 skipped=0 rescued=0 ignored=0",
			},
			wantFound:       true,
			wantUnreachable: []string{"laptop"},
		},
		{
			name: "recap failed and unreachable",
			lines: []string{
				"PLAY RECAP ***",
				"db     : ok=0 changed=0 unreachable=0 failed=1 skipped=0 rescued=0 ignored=0",
				"laptop : ok=0 changed=0 unreachable=1 failed=0 skipped=0 rescued=0 ignored=0",
			},
			wantFound:       true,
			wantUnreachable: []string{"laptop"},
			wantFailed:      []string{"db"},
		},
		{
			name: "adhoc unreachable only",
			lines: []string{
				"laptop | UNREACHABLE! => {",
				`    "unreachable": true`,
				"localhost | SUCCESS => {",
			},
			wantFound:       true,
			wantUnreachable: []string{"laptop"},
		},
		{
			name: "adhoc failed",
			lines: []string{
				"laptop | UNREACHABLE! => {",
				"localhost | FAILED | rc=1 >>",
			},
			wantFound:       true,
			wantUnreachable: []string{"laptop"},
			wantFailed:      []string{"localhost"},
		},
		{
			name:      "no record fails closed",
			lines:     []string{"an error before any host result"},
			wantFound: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, found := executor.ClassifyOutcome(tc.lines)
			if found != tc.wantFound {
				t.Fatalf("found=%v, want %v", found, tc.wantFound)
			}
			if !eq(c.Unreachable, tc.wantUnreachable) {
				t.Errorf("unreachable=%v, want %v", c.Unreachable, tc.wantUnreachable)
			}
			if !eq(c.Failed, tc.wantFailed) {
				t.Errorf("failed=%v, want %v", c.Failed, tc.wantFailed)
			}
		})
	}
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// unreachableRecap is a fake ansible that converges the reachable host, records
// the laptop unreachable, and exits 4 (RUN_UNREACHABLE_HOSTS) — exactly what a
// real run over a sleeping roamer does.
const unreachableRecap = `
echo "PLAY RECAP ****"
echo "localhost                  : ok=1    changed=0    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0"
echo "laptop                     : ok=0    changed=0    unreachable=1    failed=0    skipped=0    rescued=0    ignored=0"
exit 4
`

// failedRecap converges nobody: the reachable host's task failed (exit 2 bit)
// and the laptop is unreachable — a hard failure regardless of the flag.
const failedRecap = `
echo "PLAY RECAP ****"
echo "localhost                  : ok=0    changed=0    unreachable=0    failed=1    skipped=0    rescued=0    ignored=0"
echo "laptop                     : ok=0    changed=0    unreachable=1    failed=0    skipped=0    rescued=0    ignored=0"
exit 6
`

func runRecap(t *testing.T, script string, tolerate bool) (executor.Result, *collector) {
	t.Helper()
	bin := fakeAnsible(t, script)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var sink collector
	res, err := executor.NewAnsible(executor.AnsibleOptions{
		Binary:              bin,
		PrivateDirRoot:      t.TempDir(),
		TolerateUnreachable: tolerate,
	}).Run(ctx, executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv("/p/site.yml", "/p/hosts.ini"),
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res, &sink
}

// TestTolerateUnreachableDegrades is the driver-level item-3 gate: a
// tolerate-unreachable run whose only non-success is an unreachable host settles
// Degraded, names the host, and emits the partial event — while the true exit
// code is preserved (invariant 3).
func TestTolerateUnreachableDegrades(t *testing.T) {
	t.Parallel()
	res, sink := runRecap(t, unreachableRecap, true)
	if !res.Degraded {
		t.Fatalf("Degraded=false, want true for an unreachable-only tolerated run")
	}
	if res.ExitCode != 4 {
		t.Errorf("ExitCode=%d, want the real 4 preserved", res.ExitCode)
	}
	if !eq(res.UnreachableHosts, []string{"laptop"}) {
		t.Errorf("UnreachableHosts=%v, want [laptop]", res.UnreachableHosts)
	}
	var partial string
	for _, raw := range sink.rawJSON() {
		if strings.Contains(raw, "ansible_partial") {
			partial = raw
		}
	}
	if partial == "" {
		t.Fatalf("no ansible_partial event: %v", sink.rawJSON())
	}
	out, ok := executor.AnsiblePartialHosts(partial)
	if !ok || !eq(out.Unreachable, []string{"laptop"}) {
		t.Errorf("partial round-trip=%+v ok=%v", out, ok)
	}
}

// TestTolerateUnreachableStillFailsOnTaskFailure proves the flag never hides a
// real failure: a reachable host whose task failed keeps the run a hard failure
// even though a host was also unreachable.
func TestTolerateUnreachableStillFailsOnTaskFailure(t *testing.T) {
	t.Parallel()
	res, sink := runRecap(t, failedRecap, true)
	if res.Degraded {
		t.Fatalf("Degraded=true, want false when a reachable host's task failed")
	}
	for _, raw := range sink.rawJSON() {
		if strings.Contains(raw, "ansible_partial") {
			t.Errorf("a run with a task failure emitted a partial event: %s", raw)
		}
	}
}

// TestTolerateUnreachableOffIsStrict proves the default is unchanged: without
// the opt-in, an unreachable host is an ordinary non-zero exit, never degraded.
func TestTolerateUnreachableOffIsStrict(t *testing.T) {
	t.Parallel()
	res, _ := runRecap(t, unreachableRecap, false)
	if res.Degraded {
		t.Fatalf("Degraded=true without --tolerate-unreachable; the default must stay strict")
	}
	if res.ExitCode != 4 {
		t.Errorf("ExitCode=%d, want 4", res.ExitCode)
	}
}

// TestTolerateUnreachableNoRecapFailsClosed proves a run that errored before any
// host record is never called degraded: with nothing to prove failed==0, the
// flag cannot turn an unknown outcome into a tolerated one.
func TestTolerateUnreachableNoRecapFailsClosed(t *testing.T) {
	t.Parallel()
	res, _ := runRecap(t, "echo 'boom: could not connect to backend' >&2\nexit 4\n", true)
	if res.Degraded {
		t.Fatalf("Degraded=true with no recap; a tolerated degradation must be proven, not assumed")
	}
}

// TestAnsibleAdhocArgv covers the ad-hoc argv builder: the pattern is the sole
// positional, the module and args ride their flags, and an empty inventory adds
// no --inventory, deferring to ansible's own inventory discovery.
func TestAnsibleAdhocArgv(t *testing.T) {
	t.Parallel()
	got := executor.AnsibleAdhocArgv("all", "", "command", "hostname", "--become")
	want := []string{"ansible", "all",
		"--module-name", "command", "--args", "hostname", "--become"}
	if !eq(got, want) {
		t.Fatalf("adhoc argv=%v\nwant %v", got, want)
	}

	// A registered inventory and no args (ping/setup take none).
	got = executor.AnsibleAdhocArgv("fedora:!nas", "/p/hosts.ini", "ansible.builtin.ping", "")
	want = []string{"ansible", "fedora:!nas", "--inventory", "/p/hosts.ini",
		"--module-name", "ansible.builtin.ping"}
	if !eq(got, want) {
		t.Fatalf("adhoc argv=%v\nwant %v", got, want)
	}
}
