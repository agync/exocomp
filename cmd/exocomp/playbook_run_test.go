package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// probePlay is a real play that proves which flags reached ansible. The assert
// task fails the run unless the `proof` extra-var arrived; the two copy tasks
// are tagged so --tags selects one, and their dest is keyed by inventory_hostname
// so --limit selects the host. In check mode nothing is written, so a marker
// file's absence is proof the dry run changed nothing.
func writeProbePlay(t *testing.T, marker string) (play, inventory string) {
	t.Helper()
	dir := t.TempDir()

	inventory = filepath.Join(dir, "hosts.ini")
	writeFile(t, inventory, "hostA ansible_connection=local\nhostB ansible_connection=local\n")

	play = filepath.Join(dir, "probe.yml")
	writeFile(t, play, `- hosts: all
  gather_facts: false
  tasks:
    - name: prove the extra var arrived
      ansible.builtin.assert:
        that:
          - proof == 'xyz'
        fail_msg: extra var not received
      tags: [ta, tb]
    - name: write A
      ansible.builtin.copy:
        content: "A {{ proof }}\n"
        dest: "`+marker+`/{{ inventory_hostname }}.ta"
      tags: [ta]
    - name: write B
      ansible.builtin.copy:
        content: "B {{ proof }}\n"
        dest: "`+marker+`/{{ inventory_hostname }}.tb"
      tags: [tb]
`)
	return play, inventory
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestPlaybookRunLimitTagsExtraVarReachAnsible is the item-1 gate: --limit,
// --tags and -e key=val each demonstrably take effect in a real localhost run.
func TestPlaybookRunLimitTagsExtraVarReachAnsible(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook")

	marker := t.TempDir()
	play, inventory := writeProbePlay(t, marker)

	passthrough, vars, err := ansiblePassthrough("hostA", "ta", "", false, false, []string{"proof=xyz"})
	if err != nil {
		t.Fatalf("ansiblePassthrough: %v", err)
	}

	var out, warn bytes.Buffer
	code, err := runPlaybook(context.Background(), &out, &warn, playbookSpec{
		playbook:    play,
		inventory:   inventory,
		passthrough: passthrough,
		extraVars:   vars,
	})
	if err != nil {
		t.Fatalf("playbook run: %v\n%s%s", err, out.String(), warn.String())
	}
	if code != 0 {
		t.Fatalf("playbook run exited %d (the assert task fails when -e did not arrive):\n%s", code, out.String())
	}

	// --limit hostA: only hostA ran. --tags ta: only the .ta task ran. -e proof:
	// the file's content is the extra-var value, so its presence proves all three.
	hostATA := filepath.Join(marker, "hostA.ta")
	if !exists(hostATA) {
		t.Fatalf("hostA.ta not written; --limit/--tags/-e did not all reach ansible:\n%s", out.String())
	}
	if body, _ := os.ReadFile(hostATA); strings.TrimSpace(string(body)) != "A xyz" {
		t.Errorf("hostA.ta = %q, want \"A xyz\" (the -e value)", body)
	}
	if exists(filepath.Join(marker, "hostB.ta")) {
		t.Errorf("hostB.ta was written despite --limit hostA")
	}
	if exists(filepath.Join(marker, "hostA.tb")) {
		t.Errorf("the tb-tagged task ran despite --tags ta")
	}
}

// TestPlaybookRunExtraVarsReplayedByDaemon proves the extra-vars survive the
// job row: a daemon that claims the row — not the enqueuing CLI — runs the play
// with the operator's -e value, so --limit/--tags/-e all take effect off the
// daemon's own read of the row.
func TestPlaybookRunExtraVarsReplayedByDaemon(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook")
	bin := buildExocomp(t)
	runCLI(t, bin, "init")

	marker := t.TempDir()
	play, inventory := writeProbePlay(t, marker)

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, code := runCLIExit(t, bin, "playbook", "run", play,
		"--inventory", inventory, "--limit", "hostA", "--tags", "ta", "-e", "proof=xyz")
	if code != 0 {
		t.Fatalf("playbook run via daemon exited %d:\n%s\n%s", code, out, d.dump())
	}
	if body, _ := os.ReadFile(filepath.Join(marker, "hostA.ta")); strings.TrimSpace(string(body)) != "A xyz" {
		t.Fatalf("daemon did not replay -e proof=xyz (hostA.ta=%q):\n%s\n%s", body, out, d.dump())
	}
	if exists(filepath.Join(marker, "hostB.ta")) || exists(filepath.Join(marker, "hostA.tb")) {
		t.Errorf("--limit/--tags did not take effect on the daemon run:\n%s", d.dump())
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
}

// TestPlaybookRunExtraVarFile proves -e @file reaches ansible and that the
// values stay in the file — the argv carries only the path.
func TestPlaybookRunExtraVarFile(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook")

	marker := t.TempDir()
	play, inventory := writeProbePlay(t, marker)

	varsFile := filepath.Join(t.TempDir(), "vars.yml")
	writeFile(t, varsFile, "proof: xyz\n")

	passthrough, vars, err := ansiblePassthrough("hostA", "ta", "", false, false, []string{"@" + varsFile})
	if err != nil {
		t.Fatalf("ansiblePassthrough: %v", err)
	}
	if vars != nil {
		t.Fatalf("@file must not populate the extra-vars map: %v", vars)
	}
	// The file path travels in argv; the value does not.
	joined := strings.Join(passthrough, " ")
	if !strings.Contains(joined, "-e @"+varsFile) {
		t.Fatalf("passthrough missing the -e @file ref: %v", passthrough)
	}
	if strings.Contains(joined, "xyz") {
		t.Fatalf("the extra-var value leaked into argv: %v", passthrough)
	}

	var out, warn bytes.Buffer
	code, err := runPlaybook(context.Background(), &out, &warn, playbookSpec{
		playbook:    play,
		inventory:   inventory,
		passthrough: passthrough,
		extraVars:   vars,
	})
	if err != nil || code != 0 {
		t.Fatalf("playbook run exited %d err=%v:\n%s", code, err, out.String())
	}
	if !exists(filepath.Join(marker, "hostA.ta")) {
		t.Fatalf("-e @file did not reach ansible:\n%s", out.String())
	}
}

// TestPlaybookRunCheckEmitsDriftAndChangesNothing is the item-2 gate: --check
// --diff runs a dry run that writes no host state and emits the structured
// config-drift outcome (the 'would change' count) as a job event.
func TestPlaybookRunCheckEmitsDriftAndChangesNothing(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook")

	marker := t.TempDir()
	play, inventory := writeProbePlay(t, marker)

	passthrough, vars, err := ansiblePassthrough("hostA", "", "", true, true, []string{"proof=xyz"})
	if err != nil {
		t.Fatalf("ansiblePassthrough: %v", err)
	}

	var out, warn bytes.Buffer
	code, err := runPlaybook(context.Background(), &out, &warn, playbookSpec{
		playbook:    play,
		inventory:   inventory,
		passthrough: passthrough,
		extraVars:   vars,
	})
	if err != nil {
		t.Fatalf("playbook run --check: %v\n%s%s", err, out.String(), warn.String())
	}
	if code != 0 {
		t.Fatalf("playbook run --check exited %d:\n%s", code, out.String())
	}

	// --check made no changes: neither would-change file exists on disk.
	if exists(filepath.Join(marker, "hostA.ta")) || exists(filepath.Join(marker, "hostA.tb")) {
		t.Errorf("a --check run wrote host state:\n%s", out.String())
	}

	// The structured drift outcome reached the job events. hostA has two
	// would-change tasks (.ta and .tb both missing), so changed=2.
	events := jobEventText(t, lastJobID(t, "playbook"))
	if !strings.Contains(events, `"ansible_check"`) {
		t.Fatalf("no ansible_check outcome in the job events:\n%s", events)
	}
	if !strings.Contains(events, `"host":"hostA"`) || !strings.Contains(events, `"changed":2`) {
		t.Errorf("drift outcome missing hostA changed=2:\n%s", events)
	}

	// The human line an operator scans, and the diff --diff produced.
	if !strings.Contains(out.String(), "ansible check:") || !strings.Contains(out.String(), "would change") {
		t.Errorf("no check summary line in the run output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "+A xyz") {
		t.Errorf("--diff did not surface the would-be change:\n%s", out.String())
	}
}

// TestPlaybookRunNonCheckEmitsNoDrift proves a normal run is untouched: no
// ansible_check event, the drift surface is check-mode only.
func TestPlaybookRunNonCheckEmitsNoDrift(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook")

	marker := t.TempDir()
	play, inventory := writeProbePlay(t, marker)

	passthrough, vars, err := ansiblePassthrough("hostA", "ta", "", false, false, []string{"proof=xyz"})
	if err != nil {
		t.Fatalf("ansiblePassthrough: %v", err)
	}

	var out, warn bytes.Buffer
	if _, err := runPlaybook(context.Background(), &out, &warn, playbookSpec{
		playbook:    play,
		inventory:   inventory,
		passthrough: passthrough,
		extraVars:   vars,
	}); err != nil {
		t.Fatalf("playbook run: %v\n%s", err, out.String())
	}
	events := jobEventText(t, lastJobID(t, "playbook"))
	if strings.Contains(events, "ansible_check") {
		t.Fatalf("a non-check run emitted a drift event:\n%s", events)
	}
}

// TestAnsiblePassthroughRejectsInjection guards the validation: a crafted
// extra-var key or a control character in a value is refused before the run.
func TestAnsiblePassthroughRejectsInjection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit string
		vars  []string
	}{
		{name: "bad key", vars: []string{"proof --become=x=y"}},
		{name: "key with space", vars: []string{"a b=c"}},
		{name: "newline in value", vars: []string{"k=v\nx"}},
		{name: "newline in limit", limit: "all\n--become"},
		{name: "extra var no equals", vars: []string{"notavar"}},
		{name: "missing @file", vars: []string{"@/no/such/file/here.yml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ansiblePassthrough(tc.limit, "", "", false, false, tc.vars); err == nil {
				t.Fatalf("accepted a value it should reject: limit=%q vars=%v", tc.limit, tc.vars)
			}
		})
	}
}
