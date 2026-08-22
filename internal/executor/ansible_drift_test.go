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
