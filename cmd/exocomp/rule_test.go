package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// seedPlaybookSource points the CLI's data-dir resolution at a temp dir holding
// a store and a registered path source whose checkout exists on disk (so rule
// add's playbook validation can resolve the playbook path). hookedVia names the
// hook source to refuse: "checkout" writes the source's own .exocomp/hooks.yml,
// "override" writes the data-dir override file, anything else writes neither.
func seedPlaybookSource(t *testing.T, hookedVia string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	dir := filepath.Join(tmp, "exocomp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "plays"), 0o700); err != nil {
		t.Fatalf("mkdir checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "plays", "site.yml"), []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	if hookedVia == "checkout" {
		if err := os.MkdirAll(filepath.Join(checkout, ".exocomp"), 0o700); err != nil {
			t.Fatalf("mkdir hooks: %v", err)
		}
		if err := os.WriteFile(filepath.Join(checkout, ".exocomp", "hooks.yml"),
			[]byte("pre_run:\n  - name: ping\n    run: echo hi\n"), 0o600); err != nil {
			t.Fatalf("write hooks: %v", err)
		}
	}
	if _, err := st.CreateSource(context.Background(), store.Source{
		Name: "ops", Kind: store.SourcePath, URI: checkout, Enabled: true,
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if hookedVia == "override" {
		if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o700); err != nil {
			t.Fatalf("mkdir override dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "hooks", "ops.yml"),
			[]byte("pre_run:\n  - name: ping\n    run: echo hi\n"), 0o600); err != nil {
			t.Fatalf("write override: %v", err)
		}
	}
}

// playbookRuleArgs is the playbook action shape every rule-add case starts
// from; the refusal cases mutate it.
func playbookRuleArgs() []string {
	return []string{
		"rule", "add", "r",
		"--source", "generic",
		"--expr", `event.name == "x" && event.status == "firing"`,
		"--action", "playbook",
		"--repo-source", "ops",
		"--playbook", "plays/site.yml",
		"--limit", "laptop",
	}
}

func withoutFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func replaceFlag(args []string, flag, value string) []string {
	out := append([]string{}, args...)
	for i := 0; i < len(out); i++ {
		if out[i] == flag {
			out[i+1] = value
		}
	}
	return out
}

// TestRuleAddRefusesNotifyApprovalGate: a notify action creates no job, so
// --require-approval has nothing to gate and the combination is refused at add
// rather than silently dropped when the engine short-circuits notify.
func TestRuleAddRefusesNotifyApprovalGate(t *testing.T) {
	useDataDir(t, t.TempDir())

	out, err := run(t, "rule", "add", "page-me",
		"--expr", "true", "--action", "notify", "--require-approval")
	if err == nil {
		t.Fatalf("rule add succeeded: %s", out)
	}
	if !strings.Contains(err.Error(), "--require-approval has nothing to gate") {
		t.Fatalf("error = %q, want the notify gate refusal", err.Error())
	}
}

func TestRuleAddPlaybookAction(t *testing.T) {
	seedPlaybookSource(t, "")

	out, err := run(t, "rule", "add", "laptop-update",
		"--source", "generic",
		"--expr", `event.name == "home" && event.status == "firing"`,
		"--action", "playbook",
		"--repo-source", "ops",
		"--playbook", "plays/site.yml",
		"--limit", "laptop",
		"--tags", "update,deploy",
		"--check", "--diff",
	)
	if err != nil {
		t.Fatalf("rule add playbook: %v (%s)", err, out)
	}
	if !strings.Contains(out, "rule laptop-update (playbook) added") {
		t.Fatalf("add output = %q", out)
	}

	list, err := run(t, "rule", "list")
	if err != nil {
		t.Fatalf("rule list: %v", err)
	}
	if !strings.Contains(list, "playbook ops plays/site.yml (limit laptop)") {
		t.Fatalf("list = %q", list)
	}

	jlist, err := run(t, "rule", "list", "--output", "json")
	if err != nil {
		t.Fatalf("rule list json: %v", err)
	}
	for _, want := range []string{
		`"source":"ops"`, `"playbook":"plays/site.yml"`, `"limit":"laptop"`,
		`"tags":["update","deploy"]`, `"check":true`, `"diff":true`,
	} {
		if !strings.Contains(jlist, want) {
			t.Errorf("json list missing %s: %s", want, jlist)
		}
	}
}

func TestRuleAddPlaybookRefuses(t *testing.T) {
	seedPlaybookSource(t, "")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing repo-source", withoutFlag(playbookRuleArgs(), "--repo-source"), "needs --repo-source"},
		{"missing playbook", withoutFlag(playbookRuleArgs(), "--playbook"), "needs --playbook"},
		{"missing limit", withoutFlag(playbookRuleArgs(), "--limit"), "needs --limit"},
		{"bad limit", replaceFlag(playbookRuleArgs(), "--limit", "host\nother"), "newline"},
		{"positional argv", append(playbookRuleArgs(), "--", "extra"), "takes no argv"},
		{"unknown source", replaceFlag(playbookRuleArgs(), "--repo-source", "ghost"), "no source"},
	} {
		out, err := run(t, tc.args...)
		if err == nil {
			t.Errorf("%s: rule add succeeded, want a refusal (%s)", tc.name, out)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, want it to contain %q", tc.name, err.Error(), tc.want)
		}
	}
}

// TestRuleAddPlaybookRefusesHookedSource: a source hooked either way — its own
// checkout .exocomp/hooks.yml or the data-dir override — cannot back a rule
// playbook, because a rule never composes the lifecycle workflow.
func TestRuleAddPlaybookRefusesHookedSource(t *testing.T) {
	for _, via := range []string{"checkout", "override"} {
		seedPlaybookSource(t, via)
		out, err := run(t, playbookRuleArgs()...)
		if err == nil {
			t.Errorf("%s-hooked source: rule add succeeded, want a refusal (%s)", via, out)
			continue
		}
		if !strings.Contains(err.Error(), "has hooks") {
			t.Errorf("%s-hooked source: error = %q, want it to name the hooks refusal", via, err.Error())
		}
	}
}
