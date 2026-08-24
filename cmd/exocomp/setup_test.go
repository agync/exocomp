package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/tui"
)

// spyProvisioner records how many times Commit ran and when, so a test can
// assert the wizard writes exactly once and only at step 4.
type spyProvisioner struct {
	commits int
	last    setupInput
}

func (s *spyProvisioner) AlreadyInitialized() bool { return false }
func (s *spyProvisioner) Commit(_ context.Context, in setupInput) (string, string, error) {
	s.commits++
	s.last = in
	return "KIT-TEXT", "topic " + in.topic + " · test push sent", nil
}

func typeRunes(m setupModel, s string) setupModel {
	for _, r := range s {
		next, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = next.(setupModel)
	}
	return m
}

func press(m setupModel, code rune) setupModel {
	next, _ := m.Update(tea.KeyPressMsg{Code: code})
	return next.(setupModel)
}

// TestSetupWritesNothingUntilStep4 drives the whole flow against a spy and
// asserts Commit runs exactly once, only when step 4 is entered.
func TestSetupWritesNothingUntilStep4(t *testing.T) {
	spy := &spyProvisioner{}
	m := newSetupModel(spy, "aurora")

	m = typeRunes(m, "Dana Kim")
	m = press(m, tea.KeyEnter) // -> step 2
	if m.step != 2 {
		t.Fatalf("after step 1 enter, step = %d, want 2", m.step)
	}
	if spy.commits != 0 {
		t.Fatalf("commit ran at step %d (count %d), want none before step 4", m.step, spy.commits)
	}

	// The kit gate blocks on the exact phrase: a wrong phrase leaves step 2.
	m = typeRunes(m, "i have stored this") // wrong case
	m = press(m, tea.KeyEnter)
	if m.step != 2 {
		t.Fatalf("wrong confirmation advanced past the kit gate to step %d", m.step)
	}
	if spy.commits != 0 {
		t.Fatalf("commit ran on a failed confirmation")
	}

	// Clear the wrong phrase and type the exact one.
	for range "i have stored this" {
		m = press(m, tea.KeyBackspace)
	}
	m = typeRunes(m, tui.KitConfirmPhrase)
	m = press(m, tea.KeyEnter) // -> step 3
	if m.step != 3 {
		t.Fatalf("exact confirmation did not advance to step 3, step = %d", m.step)
	}
	if spy.commits != 0 {
		t.Fatalf("commit ran at step 3, want only at step 4")
	}

	m = press(m, tea.KeyEnter) // -> step 4, the single write
	if m.step != 4 {
		t.Fatalf("did not reach step 4, step = %d", m.step)
	}
	if spy.commits != 1 {
		t.Fatalf("commit count = %d entering step 4, want exactly 1", spy.commits)
	}
	if spy.last.operator != "Dana Kim" || spy.last.machine != "aurora" {
		t.Fatalf("commit input = %+v, want operator Dana Kim on aurora", spy.last)
	}
	if !strings.HasPrefix(spy.last.topic, "exo-aurora-") {
		t.Fatalf("generated topic = %q, want exo-aurora-…", spy.last.topic)
	}

	// A redraw of step 4 must not write again.
	m = press(m, tea.KeyEnter)
	if spy.commits != 1 {
		t.Fatalf("commit ran again on step-4 redraw, count %d", spy.commits)
	}
}

// TestSetupEscFromStep1LeavesNoTrace confirms esc on step 1 quits without a
// commit — the escape hatch the copy promises.
func TestSetupEscFromStep1LeavesNoTrace(t *testing.T) {
	spy := &spyProvisioner{}
	m := newSetupModel(spy, "aurora")
	m = press(m, tea.KeyEscape)
	if !m.quitting {
		t.Fatal("esc on step 1 did not quit")
	}
	if spy.commits != 0 {
		t.Fatal("esc on step 1 wrote to disk")
	}
}

// TestSetupRealProvisionerDataDirEmptyUntilStep4 is the end-to-end proof
// against the real provisioner: the data dir stays empty until step 4, then
// secrets.Init lands a host key.
func TestSetupRealProvisionerDataDirEmptyUntilStep4(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	prov := &dataDirProvisioner{dir: dir}
	m := newSetupModel(prov, "aurora")

	m = press(m, tea.KeyEnter)             // step 2
	m = typeRunes(m, tui.KitConfirmPhrase) // confirm
	m = press(m, tea.KeyEnter)             // step 3
	if _, err := os.Stat(filepath.Join(dir, "host.key")); !os.IsNotExist(err) {
		t.Fatalf("host.key exists before step 4 (err=%v) — the flow wrote early", err)
	}

	m = press(m, tea.KeyEnter) // step 4: the single write
	if m.err != nil {
		t.Fatalf("commit failed: %v", m.err)
	}
	if _, err := os.Stat(filepath.Join(dir, "host.key")); err != nil {
		t.Fatalf("host.key missing after step 4: %v", err)
	}
	if m.kitText == "" {
		t.Fatal("hand-over step produced no recovery kit text")
	}
}

// TestCommitFailureAfterHostKeyLeavesRerunPossible reproduces finding #8:
// secrets.Init lands host.key first, then Commit opens the store and persists
// config. If that later step fails, host.key is already on disk. The "nothing
// partial was left behind" promise requires either the identity to be rolled
// back or the failure to leave the host genuinely re-runnable — not a state
// that claims cleanliness while AlreadyInitialized blocks every retry.
func TestCommitFailureAfterHostKeyLeavesRerunPossible(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-create exocomp.db as a directory so openStoreAt's store.Open fails
	// with a real, unforced error — the store-open/persist step Commit runs
	// after secrets.Init has already written host.key.
	if err := os.Mkdir(filepath.Join(dir, "exocomp.db"), 0o700); err != nil {
		t.Fatal(err)
	}

	prov := &dataDirProvisioner{dir: dir}
	_, _, err := prov.Commit(context.Background(), setupInput{
		machine:  "aurora",
		operator: "Dana Kim",
		topic:    "exo-aurora-abc",
	})
	if err == nil {
		t.Fatal("Commit succeeded despite the poisoned store path; test setup is wrong")
	}

	if _, statErr := os.Stat(filepath.Join(dir, "host.key")); !os.IsNotExist(statErr) {
		t.Fatalf("host.key stat after rollback = %v, want IsNotExist (Rollback removes it)", statErr)
	}
	if prov.AlreadyInitialized() {
		t.Fatalf("Commit failed after writing host.key (err=%v) but left the host "+
			"marked AlreadyInitialized — setup can never be re-run, yet nothing "+
			"beyond the identity was actually persisted", err)
	}
}
