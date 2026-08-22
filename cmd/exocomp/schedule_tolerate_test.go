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

// materializeRoamingFleet registers a `tf:<name>` standing inventory with one
// reachable local host and one deliberately-unreachable host (ssh to a refused
// port), the roaming-laptop shape a scheduled converge has to tolerate.
func materializeRoamingFleet(t *testing.T, name string) {
	t.Helper()
	body := `{"all": {"hosts": {
      "localhost": {"ansible_connection": "local"},
      "laptop": {"ansible_host": "127.0.0.1", "ansible_connection": "ssh",
                 "ansible_port": 1, "ansible_user": "nobody",
                 "ansible_ssh_common_args": "-o ConnectTimeout=2 -o BatchMode=yes"}
    }}}`
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

// roamJobs returns each "roam" schedule job with its state and joined event text,
// so a scan finds a settled fire whatever else is queued behind it.
func roamJobs(t *testing.T) []store.Job {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	jobs, err := st.ListJobs(context.Background(), store.JobFilter{Runbook: "roam"})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	return jobs
}

// TestScheduledTolerateUnreachablePlaySettlesPartial is the schedule gate for the
// unreachable-tolerant outcome: a `schedule add --kind play --tolerate-unreachable`
// against a fleet with an offline host fires, converges the online host, and
// settles the fire DEGRADED (succeeded + partial, the offline host named) instead
// of failing and paging every cron tick.
func TestScheduledTolerateUnreachablePlaySettlesPartial(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook")
	bin := buildExocomp(t)
	runCLI(t, bin, "init")

	materializeRoamingFleet(t, "roamlab")

	play := filepath.Join(t.TempDir(), "site.yml")
	writeFile(t, play, `- hosts: all
  gather_facts: false
  tasks:
    - name: converge
      ansible.builtin.ping:
`)

	runCLI(t, bin, "schedule", "add", "roam", "--kind", "play",
		"--target", "tf:roamlab", "--tolerate-unreachable", "--cron", "@every 3s", "--", play)

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	var degraded store.Job
	waitFor(t, "a scheduled tolerate-unreachable fire to settle degraded", 60*time.Second, func() bool {
		for _, job := range roamJobs(t) {
			if job.State == store.StateSucceeded && job.Partial {
				degraded = job
				return true
			}
			if job.State == store.StateFailed {
				t.Fatalf("a scheduled tolerate-unreachable fire FAILED (job %d); it must settle degraded:\n%s",
					job.ID, d.dump())
			}
		}
		return false
	})

	events := jobEventText(t, degraded.ID)
	if !strings.Contains(events, "ansible_partial") || !strings.Contains(events, "laptop") {
		t.Fatalf("degraded fire (job %d) did not name the offline host:\n%s\n%s", degraded.ID, events, d.dump())
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
}
