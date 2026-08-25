package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// materializeStandingInventory registers a `tf:<name>` inventory holding one
// localhost host over the local connection, the way a successful apply's
// capture would, so a scheduled play has real hosts to converge.
func materializeStandingInventory(t *testing.T, name string) {
	t.Helper()
	body := `{"all": {"hosts": {"localhost": {"ansible_connection": "local"}}}}`
	path := filepath.Join(t.TempDir(), name+".json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.MaterializeInventory(context.Background(), "tf:"+name, path); err != nil {
		t.Fatalf("materialize inventory: %v", err)
	}
}

// driftJobEvents returns the joined event text of every job the "drift"
// schedule has produced, so a scan finds the fired check whatever else is
// queued behind it.
func driftJobEvents(t *testing.T) []string {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	jobs, err := st.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	var out []string
	for _, job := range jobs {
		if job.Runbook != "drift" {
			continue
		}
		events, err := st.ListJobEvents(context.Background(), job.ID)
		if err != nil {
			continue
		}
		var b strings.Builder
		for _, e := range events {
			b.WriteString(e.RawJSON)
			b.WriteString("\n")
		}
		out = append(out, b.String())
	}
	return out
}

// TestScheduledCheckPlayFiresAndEmitsDrift is the item-2 schedule gate: a
// `schedule add --kind play --check` registered on a fast cron fires check-mode
// plays that each emit the structured config-drift outcome, and the run touches
// no host state — the config twin of a tofu-plan drift schedule.
func TestScheduledCheckPlayFiresAndEmitsDrift(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook")
	bin := buildExocomp(t)
	runCLI(t, bin, "init")

	materializeStandingInventory(t, "driftlab")

	// A play whose one task would create a file: in check mode every fire
	// reports it as a would-change without writing it.
	marker := t.TempDir()
	play := filepath.Join(t.TempDir(), "drift.yml")
	writeFile(t, play, `- hosts: all
  gather_facts: false
  tasks:
    - name: would change
      ansible.builtin.copy:
        content: "drift\n"
        dest: "`+marker+`/{{ inventory_hostname }}.state"
`)

	// Register the config-drift schedule and confirm the row is check-mode.
	runCLI(t, bin, "schedule", "add", "drift", "--kind", "play",
		"--target", "tf:driftlab", "--cron", "@every 3s", "--", play, "--check")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "disabled")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	// A fast cron keeps enqueueing, so any single job's events are the proof —
	// not the newest row, which may still be queued. Scan every drift job for a
	// fired check that carried the outcome.
	var outcome string
	waitFor(t, "a fired check job to emit the drift outcome", 60*time.Second, func() bool {
		for _, ev := range driftJobEvents(t) {
			if strings.Contains(ev, "ansible_check") {
				outcome = ev
				return true
			}
		}
		return false
	})

	if !strings.Contains(outcome, `"host":"localhost"`) || !strings.Contains(outcome, `"changed":1`) {
		t.Errorf("scheduled check fire missing localhost changed=1 drift outcome:\n%s\n%s", outcome, d.dump())
	}
	// Check mode changed nothing on disk.
	if exists(filepath.Join(marker, "localhost.state")) {
		t.Errorf("a scheduled check play wrote host state:\n%s", d.dump())
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
}
