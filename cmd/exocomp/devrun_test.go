package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunDevRunReportsFinalState(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	var out bytes.Buffer
	err := runDevRun(context.Background(), &out, devRunSpec{argv: []string{"/bin/echo", "hello"}})
	if err != nil {
		t.Fatalf("devrun: %v", err)
	}

	got := out.String()
	for _, want := range []string{"state: succeeded", "exit: 0", "[info] hello"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q does not contain %q", got, want)
		}
	}
}

func TestRunDevRunFailsWithTheJob(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	var out bytes.Buffer
	err := runDevRun(context.Background(), &out, devRunSpec{
		argv:    []string{"/bin/sleep", "30"},
		timeout: 150 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("devrun returned nil for a timed-out job")
	}
	if !strings.Contains(out.String(), "state: timed_out") {
		t.Errorf("output = %q", out.String())
	}
}
