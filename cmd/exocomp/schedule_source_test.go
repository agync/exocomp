package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// addSource registers a path source over a fresh checkout and returns it.
func addSource(t *testing.T, name string) string {
	t.Helper()
	checkout := t.TempDir()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.CreateSource(context.Background(),
		store.Source{Name: name, Kind: store.SourcePath, URI: checkout, Enabled: true}); err != nil {
		t.Fatalf("create source %q: %v", name, err)
	}
	return checkout
}

// TestScheduleAddPlayStoresTheSource: --source on a play schedule is recorded
// on the row, beside the unchanged playbook and passthrough, and reaches the
// JSON listing.
func TestScheduleAddPlayStoresTheSource(t *testing.T) {
	useDataDir(t, t.TempDir())
	addSource(t, "infra")

	if out, err := runScheduleCmd(t, "add", "converge", "--kind", "play", "--target", "tf:edge",
		"--cron", "@every 1h", "--source", "infra", "--check", "--", "plays/site.yml"); err != nil {
		t.Fatalf("schedule add --source: %v\n%s", err, out)
	}
	rows := schedules(t)
	if len(rows) != 1 {
		t.Fatalf("schedules = %d, want 1", len(rows))
	}
	if rows[0].Args.Source != "infra" {
		t.Errorf("source = %q, want infra", rows[0].Args.Source)
	}
	if got := strings.Join(rows[0].Args.Argv, " "); got != "plays/site.yml --check --diff" {
		t.Errorf("argv = %q, want the relative playbook and its passthrough", got)
	}

	t.Setenv("EXOCOMP_OUTPUT", "json")
	out, err := runScheduleCmd(t, "list")
	if err != nil {
		t.Fatalf("schedule list: %v", err)
	}
	if !strings.Contains(out, `"source":"infra"`) && !strings.Contains(out, `"source": "infra"`) {
		t.Errorf("json listing = %s, want the source named", out)
	}
}

// TestScheduleAddPlayWithoutSourceStoresNone: omitting the flag keeps the
// original shape, an as-given playbook path and no source.
func TestScheduleAddPlayWithoutSourceStoresNone(t *testing.T) {
	useDataDir(t, t.TempDir())

	if out, err := runScheduleCmd(t, "add", "converge", "--kind", "play", "--target", "tf:edge",
		"--cron", "@every 1h", "--", "/srv/site.yml"); err != nil {
		t.Fatalf("schedule add: %v\n%s", err, out)
	}
	rows := schedules(t)
	if len(rows) != 1 || rows[0].Args.Source != "" || rows[0].Args.Argv[0] != "/srv/site.yml" {
		t.Fatalf("rows = %+v, want one sourceless schedule with the playbook as given", rows)
	}
}

// TestScheduleAddSourceUnknownIsRefused: a typo fails at registration, not at
// the first fire, and nothing is written.
func TestScheduleAddSourceUnknownIsRefused(t *testing.T) {
	useDataDir(t, t.TempDir())

	out, err := runScheduleCmd(t, "add", "converge", "--kind", "play", "--target", "tf:edge",
		"--cron", "@every 1h", "--source", "ghost", "--", "site.yml")
	if err == nil {
		t.Fatalf("schedule add accepted an unregistered source:\n%s", out)
	}
	if !strings.Contains(err.Error(), `no source "ghost"`) {
		t.Errorf("error = %v, want one naming the missing source", err)
	}
	if rows := schedules(t); len(rows) != 0 {
		t.Errorf("the refused schedule was written anyway: %+v", rows)
	}
}

// TestScheduleAddSourceRejectedForNonPlay: only a play runs from a checkout; on
// any other kind --source is a mistake, not silently dropped.
func TestScheduleAddSourceRejectedForNonPlay(t *testing.T) {
	useDataDir(t, t.TempDir())
	addSource(t, "infra")

	for _, args := range [][]string{
		{"add", "tick", "--kind", "shell", "--cron", "@every 1h", "--source", "infra", "--", "/bin/true"},
		{"add", "nightly", "--kind", "backup", "--cron", "@every 1h", "--source", "infra"},
		{"add", "dr", "--kind", "drill", "--cron", "@every 1h", "--source", "infra"},
	} {
		out, err := runScheduleCmd(t, args...)
		if err == nil {
			t.Fatalf("%v accepted --source:\n%s", args, out)
		}
		if !strings.Contains(err.Error(), "--source applies to kind play") {
			t.Errorf("%v error = %v, want one saying --source applies to kind play", args, err)
		}
	}
	if rows := schedules(t); len(rows) != 0 {
		t.Errorf("a refused schedule was written anyway: %+v", rows)
	}
}

// TestScheduleAddSourceRefusesAPlaybookThatCannotRun: with a source the playbook
// is relative to the checkout, so an absolute path or one that climbs out of it
// is refused up front instead of failing at every fire.
func TestScheduleAddSourceRefusesAPlaybookThatCannotRun(t *testing.T) {
	useDataDir(t, t.TempDir())
	addSource(t, "infra")

	for playbook, want := range map[string]string{
		"/srv/site.yml":           "relative to the checkout",
		"nested/../../escape.yml": "outside the source checkout",
	} {
		out, err := runScheduleCmd(t, "add", "converge", "--kind", "play", "--target", "tf:edge",
			"--cron", "@every 1h", "--source", "infra", "--", playbook)
		if err == nil {
			t.Fatalf("schedule add accepted playbook %q with --source:\n%s", playbook, out)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("playbook %q: error = %v, want one containing %q", playbook, err, want)
		}
	}
	if rows := schedules(t); len(rows) != 0 {
		t.Errorf("a refused schedule was written anyway: %+v", rows)
	}
}

// TestScheduleAddSourceRefusesAHookedSource: an unattended play never composes a
// source's lifecycle, so a hooked source is refused at add as it is for a rule.
func TestScheduleAddSourceRefusesAHookedSource(t *testing.T) {
	useDataDir(t, t.TempDir())
	checkout := addSource(t, "infra")
	if err := os.MkdirAll(filepath.Join(checkout, ".exocomp"), 0o700); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".exocomp", "hooks.yml"),
		[]byte("pre_run:\n  - name: ping\n    run: echo hi\n"), 0o600); err != nil {
		t.Fatalf("write hooks: %v", err)
	}

	out, err := runScheduleCmd(t, "add", "converge", "--kind", "play", "--target", "tf:edge",
		"--cron", "@every 1h", "--source", "infra", "--", "site.yml")
	if err == nil {
		t.Fatalf("schedule add accepted a hooked source:\n%s", out)
	}
	if !strings.Contains(err.Error(), "hooks or a gate") {
		t.Errorf("error = %v, want the hooks refusal", err)
	}
	if rows := schedules(t); len(rows) != 0 {
		t.Errorf("the refused schedule was written anyway: %+v", rows)
	}
}
