package executor_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

// recapScript is a fake ansible-playbook that prints a PLAY RECAP with one
// drifting host and one clean host, as a real `--check --diff` run would.
const recapScript = `
echo "PLAY [all] *****"
echo "PLAY RECAP *********************************************************************"
echo "web1                       : ok=5    changed=2    unreachable=0    failed=0    skipped=1    rescued=0    ignored=0"
echo "web2                       : ok=5    changed=0    unreachable=0    failed=0    skipped=1    rescued=0    ignored=0"
`

// findCheckOutcome pulls the ansible_check payload out of the collected events.
func findCheckOutcome(t *testing.T, sink *collector) executor.AnsibleCheckOutcome {
	t.Helper()
	for _, raw := range sink.rawJSON() {
		var env struct {
			Outcome *executor.AnsibleCheckOutcome `json:"ansible_check"`
		}
		if json.Unmarshal([]byte(raw), &env) == nil && env.Outcome != nil {
			return *env.Outcome
		}
	}
	t.Fatalf("no ansible_check outcome in events: %v", sink.rawJSON())
	return executor.AnsibleCheckOutcome{}
}

// TestAnsibleCheckEmitsDrift is the 8c gate: a `--check` run parses its PLAY
// RECAP into a structured, resource-level drift event.
func TestAnsibleCheckEmitsDrift(t *testing.T) {
	t.Parallel()
	bin := fakeAnsible(t, recapScript)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var sink collector
	res, err := executor.NewAnsible(executor.AnsibleOptions{
		Binary:         bin,
		PrivateDirRoot: t.TempDir(),
	}).Run(ctx, executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv("/p/site.yml", "", "--check", "--diff"),
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}

	outcome := findCheckOutcome(t, &sink)
	if outcome.Operation != "check" {
		t.Errorf("operation = %q, want check", outcome.Operation)
	}
	if outcome.CleanHosts != 1 {
		t.Errorf("clean hosts = %d, want 1 (web2)", outcome.CleanHosts)
	}
	if len(outcome.Drift) != 1 || outcome.Drift[0].Host != "web1" || outcome.Drift[0].Changed != 2 {
		t.Fatalf("drift = %+v, want web1 changed=2", outcome.Drift)
	}

	// The human line reads the way an operator scanning the log expects.
	var msg string
	for _, m := range sink.msgs(executor.LevelSystem) {
		if strings.HasPrefix(m, "ansible check:") {
			msg = m
		}
	}
	if !strings.Contains(msg, "1 host(s) clean") || !strings.Contains(msg, "host web1: 2 task(s) would change") {
		t.Errorf("check message = %q", msg)
	}
}

// TestAnsibleNonCheckNoDriftEvent proves a normal (non-check) run behaves
// exactly as before: no recap parsing, no ansible_check event — even when the
// play happens to print a recap.
func TestAnsibleNonCheckNoDriftEvent(t *testing.T) {
	t.Parallel()
	bin := fakeAnsible(t, recapScript)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var sink collector
	if _, err := executor.NewAnsible(executor.AnsibleOptions{
		Binary:         bin,
		PrivateDirRoot: t.TempDir(),
	}).Run(ctx, executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv("/p/site.yml", ""),
	}, &sink); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, raw := range sink.rawJSON() {
		if strings.Contains(raw, "ansible_check") {
			t.Fatalf("a non-check run emitted a drift event: %s", raw)
		}
	}
}

// TestAnsibleCheckChanges is the reactive seam: the outcome a check run put in
// its event's raw_json reads back through the same shape TofuPlanChanges gives
// the tofu drift sweep, so a future AnsibleDrift sweep can mirror SweepDrift.
func TestAnsibleCheckChanges(t *testing.T) {
	t.Parallel()

	outcome := executor.AnsibleCheckOutcome{
		Operation:  "check",
		Changes:    true,
		CleanHosts: 1,
		Drift:      []executor.AnsibleHostDrift{{Host: "web1", Changed: 2}},
	}
	raw, err := json.Marshal(map[string]executor.AnsibleCheckOutcome{"ansible_check": outcome})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, ok := executor.AnsibleCheckChanges(string(raw))
	if !ok {
		t.Fatal("ok = false for a real ansible_check payload")
	}
	if !got.Changes || got.CleanHosts != 1 || len(got.Drift) != 1 || got.Drift[0].Host != "web1" {
		t.Errorf("outcome = %+v, want the round-trip of what was emitted", got)
	}

	// A non-check event (a tofu outcome, or nothing) reads as not-present, so a
	// sweep skips it rather than reporting a phantom clean run.
	for _, raw := range []string{"", `{"tofu":{"operation":"plan","changes":true}}`, "not json"} {
		if _, ok := executor.AnsibleCheckChanges(raw); ok {
			t.Errorf("ok = true for a non-ansible_check payload %q", raw)
		}
	}
}
