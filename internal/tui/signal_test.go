package tui

import (
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestSignalLineDeadManAge proves the footer's dead-man segment reports the
// newest succeeded backup run's age, and "--" honestly when none has ever
// succeeded. The golden Home frames pin their own signal string; this test
// covers the live signalLine.
func TestSignalLineDeadManAge(t *testing.T) {
	finished := time.Now().Add(-25 * time.Minute)

	m := New(openTestStore(t))
	m.runs = map[int64]store.BackupRun{
		1: {State: store.BackupSucceeded, FinishedAt: &finished},
	}
	got := m.signalLine()
	if !strings.Contains(got, "dead-man 25m") || !strings.Contains(got, "notify loud") {
		t.Errorf("signalLine = %q, want a fresh dead-man age and loud notify", got)
	}

	none := New(openTestStore(t))
	none.runs = map[int64]store.BackupRun{
		1: {State: store.BackupFailed, FinishedAt: &finished},
	}
	if got := none.signalLine(); !strings.Contains(got, "dead-man --") {
		t.Errorf("signalLine with no succeeded run = %q, want dead-man --", got)
	}
}
