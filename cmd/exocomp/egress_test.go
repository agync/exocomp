package main

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/hooks"
)

// TestParseEgressPorts: --egress values parse to a validated, de-duplicated,
// sorted port set; a non-integer or a port outside 1..65535 is refused at the
// parse, so a typo never reaches a job. Widening the range check in
// store.NormalizeEgressPorts fails the junk cases here.
func TestParseEgressPorts(t *testing.T) {
	t.Parallel()

	got, err := parseEgressPorts([]string{"2222", "443", "2222"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !slices.Equal(got, []int{443, 2222}) {
		t.Fatalf("parseEgressPorts = %v, want [443 2222] (deduped, sorted)", got)
	}

	// cobra's StringSliceVar already splits a comma list into tokens.
	comma, err := parseEgressPorts([]string{"2222,443"})
	if err != nil {
		t.Fatalf("parse comma list: %v", err)
	}
	if !slices.Equal(comma, []int{443, 2222}) {
		t.Fatalf("comma list = %v, want [443 2222]", comma)
	}

	for _, tc := range []struct {
		name   string
		values []string
	}{
		{"non-integer", []string{"abc"}},
		{"zero", []string{"0"}},
		{"negative", []string{"-1"}},
		{"too large", []string{"70000"}},
		{"mixed junk", []string{"2222", "70000"}},
	} {
		if _, err := parseEgressPorts(tc.values); err == nil {
			t.Errorf("%s: parseEgressPorts(%v) accepted junk", tc.name, tc.values)
		}
	}
}

// TestComposeRunStepsCarriesEgress: `run --egress` carries its declared ports
// onto the play step of the composed workflow, the same way the other run
// options do — a hook/gate composed run must not silently drop the grant.
func TestComposeRunStepsCarriesEgress(t *testing.T) {
	hk := hooks.Hooks{PreRun: []hooks.Hook{{Name: "prep", Run: "true"}}}
	steps, err := composeRunSteps("/src", false, "/src/site.yml", "", "",
		runSpec{sourceName: "infra", egressPorts: []int{2222}}, hk)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("want pre-hook + play steps, got %d", len(steps))
	}
	if !slices.Equal(steps[1].Spec.EgressPorts, []int{2222}) {
		t.Fatalf("play step EgressPorts = %v, want [2222]", steps[1].Spec.EgressPorts)
	}
}

// TestPlaybookRunEgressPortsReachJobRow is the plain local path's no-silent-drop
// proof: a playbook run declared with egress ports lands them on the job row,
// where a daemon that claims the run grants them to the sandbox. The run itself
// is unsandboxed (the disabled tier the CLI tests pin), so the assertion is on
// the stored grant, not on connectivity.
func TestPlaybookRunEgressPortsReachJobRow(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook")

	dir := t.TempDir()
	play := filepath.Join(dir, "site.yml")
	writeFile(t, play,
		"- hosts: localhost\n  gather_facts: false\n  tasks:\n"+
			"    - name: noop\n      ansible.builtin.command: /bin/true\n      changed_when: false\n")
	inventory := filepath.Join(dir, "hosts.ini")
	writeFile(t, inventory, "localhost ansible_connection=local\n")

	var out, warn bytes.Buffer
	code, err := runPlaybook(context.Background(), &out, &warn, playbookSpec{
		playbook:    play,
		inventory:   inventory,
		egressPorts: []int{2222},
	})
	if err != nil || code != 0 {
		t.Fatalf("playbook run: code=%d err=%v\n%s", code, err, out.String())
	}

	id := lastJobID(t, "playbook")
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	job, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(job.Args.EgressPorts, []int{2222}) {
		t.Fatalf("job EgressPorts = %v, want [2222]", job.Args.EgressPorts)
	}
}

// TestPlaybookRunJunkEgressRefusedAtParse: a junk --egress value fails before
// any job is created, at the parse — not at the run.
func TestPlaybookRunJunkEgressRefusedAtParse(t *testing.T) {
	useDataDir(t, t.TempDir())

	for _, junk := range []string{"abc", "0", "70000"} {
		_, _, err := runRoot(t, []string{"playbook", "run", "site.yml", "--egress", junk})
		if err == nil {
			t.Fatalf("--egress %s was accepted", junk)
		}
		if !strings.Contains(err.Error(), "egress") {
			t.Errorf("--egress %s error = %v, want it to name egress", junk, err)
		}
	}
}

// seedPlaybookRuleSource stands up the data dir + source + playbook a playbook
// rule-add needs (the same shape rule_test.go's seedPlaybookSource provides),
// for the egress assertions below.
func seedEgressRuleSource(t *testing.T) {
	t.Helper()
	seedPlaybookSource(t, "")
}

// TestRuleAddPlaybookEgressThreadsToRule: `rule add ... --action playbook
// --egress 2222` stores the declared ports on the rule action, so a fired rule
// runs its play with the same grant a CLI run gets.
func TestRuleAddPlaybookEgressThreadsToRule(t *testing.T) {
	seedEgressRuleSource(t)

	out, err := run(t, "rule", "add", "laptop-update",
		"--source", "generic",
		"--expr", `event.name == "home" && event.status == "firing"`,
		"--action", "playbook",
		"--repo-source", "ops",
		"--playbook", "plays/site.yml",
		"--limit", "laptop",
		"--egress", "2222",
	)
	if err != nil {
		t.Fatalf("rule add playbook --egress: %v (%s)", err, out)
	}
	if !strings.Contains(out, "rule laptop-update (playbook) added") {
		t.Fatalf("add output = %q", out)
	}

	// The stored rule carries the declared egress on its action, so the reactive
	// builder reads it back when the rule fires.
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	rule, err := st.GetRule(context.Background(), "laptop-update")
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if !slices.Equal(rule.Action.Egress, []int{2222}) {
		t.Fatalf("stored rule Egress = %v, want [2222]", rule.Action.Egress)
	}
}

// TestRuleAddPlaybookRefusesJunkEgress: a non-integer or out-of-range port on a
// playbook rule is refused at add, and no rule is created.
func TestRuleAddPlaybookRefusesJunkEgress(t *testing.T) {
	seedEgressRuleSource(t)

	for _, junk := range []string{"abc", "0", "70000"} {
		_, err := run(t, "rule", "add", "junk-egress",
			"--source", "generic",
			"--expr", `event.name == "x" && event.status == "firing"`,
			"--action", "playbook",
			"--repo-source", "ops",
			"--playbook", "plays/site.yml",
			"--limit", "laptop",
			"--egress", junk,
		)
		if err == nil {
			t.Fatalf("--egress %s was accepted by rule add", junk)
		}
		if !strings.Contains(err.Error(), "egress") {
			t.Errorf("--egress %s error = %v, want it to name egress", junk, err)
		}
	}

	jlist, err := run(t, "rule", "list", "--output", "json")
	if err != nil {
		t.Fatalf("rule list json: %v", err)
	}
	if strings.Contains(jlist, "junk-egress") {
		t.Fatalf("a junk-egress rule was created: %s", jlist)
	}
}
