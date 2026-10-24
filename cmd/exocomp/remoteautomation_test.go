package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// With the daemon as the default remote, schedule and rule verbs change the
// daemon's store, a play schedule and a playbook rule resolve against the
// daemon's source checkout, and this machine keeps no data dir.
func TestScheduleAndRuleVerbsChangeTheDaemonStore(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")
	ctx := context.Background()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "site.yml"), []byte("- hosts: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.CreateSource(ctx, store.Source{Name: "infra", Kind: store.SourcePath, URI: root, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		args []string
		want string
	}{
		{[]string{"schedule", "add", "nightly", "--cron", "0 0 3 * * *", "--kind", "play", "--target", "fleet",
			"--source", "infra", "--", "site.yml"}, "schedule nightly (play) added: 0 0 3 * * *"},
		{[]string{"schedule", "list"}, "nightly"},
		{[]string{"schedule", "disable", "nightly"}, "schedule nightly disabled"},
		{[]string{"rule", "add", "converge", "--expr", `event.name == "x"`, "--action", "playbook",
			"--repo-source", "infra", "--playbook", "site.yml", "--limit", "web", "--egress", "443"},
			"rule converge (playbook) added"},
		{[]string{"rule", "add", "plan", "--expr", "true", "--action", "tofu-apply", "--workspace", "edge"},
			"note: tofu-apply is destructive"},
		{[]string{"rule", "list"}, "playbook infra site.yml (limit web)"},
		{[]string{"rule", "list"}, "tofu-apply edge"},
		{[]string{"rule", "disable", "converge"}, "rule converge disabled"},
	}
	for _, step := range steps {
		out, _, err := runRoot(t, step.args)
		if err != nil {
			t.Fatalf("%v: %v\n%s", step.args, err, out)
		}
		if !strings.Contains(out, step.want) {
			t.Fatalf("%v printed %q, want %q", step.args, out, step.want)
		}
	}

	schedules, err := d.st.ListSchedules(ctx, false)
	if err != nil || len(schedules) != 1 || schedules[0].Args.Source != "infra" || schedules[0].Enabled {
		t.Fatalf("daemon schedules = %+v (%v), want nightly from infra, disabled", schedules, err)
	}
	rules, err := d.st.ListRules(ctx, false)
	if err != nil || len(rules) != 2 {
		t.Fatalf("daemon rules = %+v (%v)", rules, err)
	}

	for _, args := range [][]string{{"schedule", "rm", "nightly"}, {"rule", "rm", "converge"}, {"rule", "rm", "plan"}} {
		if out, _, err := runRoot(t, args); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if s, _ := d.st.ListSchedules(ctx, false); len(s) != 0 {
		t.Fatal("schedule rm left the daemon's schedule")
	}
	if r, _ := d.st.ListRules(ctx, false); len(r) != 0 {
		t.Fatal("rule rm left the daemon's rules")
	}
	assertNoLocalStore(t)
}

// The daemon refuses what its scheduler or engine could never run, and stores
// nothing.
func TestRemoteScheduleAndRuleRefusals(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"schedule", "add", "bad", "--cron", "not a cron", "--kind", "shell", "--", "true"}, "not a cron expression"},
		{[]string{"schedule", "add", "p", "--cron", "@every 1h", "--kind", "play", "--target", "fleet",
			"--source", "absent", "--", "site.yml"}, "absent"},
		{[]string{"rule", "add", "r", "--expr", "event.(", "--action", "notify"}, "rule add"},
		{[]string{"schedule", "add", "o", "--cron", "@every 1h", "--kind", "play", "--target", "fleet",
			"--", "site.yml", "-e", "x=1"}, "with a remote selected a play schedule takes its playbook"},
		{[]string{"rule", "add", "r", "--expr", "true", "--action", "playbook", "--repo-source", "absent",
			"--playbook", "site.yml", "--limit", "web", "--ssh-identity", "/srv/key"}, "not allowed"},
	}
	for _, tc := range cases {
		_, _, err := runRoot(t, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: want a refusal containing %q, got %v", tc.args, tc.want, err)
		}
	}
	if s, _ := d.st.ListSchedules(context.Background(), false); len(s) != 0 {
		t.Fatal("a refused schedule was stored")
	}
	if r, _ := d.st.ListRules(context.Background(), false); len(r) != 0 {
		t.Fatal("a refused rule was stored")
	}
	assertNoLocalStore(t)
}
