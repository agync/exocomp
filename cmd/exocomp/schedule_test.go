package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// runScheduleCmd executes one `schedule` invocation and returns its combined
// output with the error it reported.
func runScheduleCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	cmd := newScheduleCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)

	// Execute first: a return statement evaluates its operands left to right,
	// so reading the buffer in the return would read it before the command ran.
	err := cmd.Execute()
	return out.String(), err
}

func schedules(t *testing.T) []store.Schedule {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	rows, err := st.ListSchedules(context.Background(), false)
	if err != nil {
		t.Fatalf("list schedules: %v", err)
	}
	return rows
}

// TestScheduleAddRejectsAnUnparseableCron keeps a row that can never fire out
// of the database: the daemon would only log it, once per boot, forever.
func TestScheduleAddRejectsAnUnparseableCron(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	out, err := runScheduleCmd(t, "add", "junk", "--kind", "shell",
		"--cron", "not a cron at all", "--", "/bin/true")
	if err == nil {
		t.Fatalf("schedule add accepted an unparseable cron expression:\n%s", out)
	}
	if !strings.Contains(err.Error(), "cron expression") {
		t.Errorf("error = %v, want one naming the cron expression", err)
	}
	if rows := schedules(t); len(rows) != 0 {
		t.Errorf("the refused schedule was written anyway: %+v", rows)
	}
}

func TestScheduleAddAcceptsTheScheduler(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	for _, expr := range []string{"@every 30s", "@daily", "0 30 3 * * *"} {
		if _, err := runScheduleCmd(t, "add", "s"+expr[:2], "--kind", "shell",
			"--cron", expr, "--", "/bin/true"); err != nil {
			t.Fatalf("schedule add --cron %q: %v", expr, err)
		}
		if _, err := runScheduleCmd(t, "rm", "s"+expr[:2]); err != nil {
			t.Fatalf("schedule rm: %v", err)
		}
	}
}

// TestScheduleAddRejectsConcurrencyKeyForBackup: a backup fire keys each leg
// by its target, so a key given here would be silently dropped.
func TestScheduleAddRejectsConcurrencyKeyForBackup(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	out, err := runScheduleCmd(t, "add", "nightly", "--kind", "backup",
		"--cron", "@every 1h", "--concurrency-key", "mine")
	if err == nil {
		t.Fatalf("schedule add accepted --concurrency-key for kind backup:\n%s", out)
	}
	if !strings.Contains(err.Error(), "concurrency-key") {
		t.Errorf("error = %v, want one naming the flag", err)
	}
	if rows := schedules(t); len(rows) != 0 {
		t.Errorf("the refused schedule was written anyway: %+v", rows)
	}

	if _, err := runScheduleCmd(t, "add", "nightly", "--kind", "backup",
		"--cron", "@every 1h"); err != nil {
		t.Fatalf("schedule add without the flag: %v", err)
	}
}
