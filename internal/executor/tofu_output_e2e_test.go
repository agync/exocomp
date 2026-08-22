package executor_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// sensitiveCanary is the value a sensitive-marked output holds. It must never
// reach the materialized inventory or the durable event log.
const sensitiveCanary = "exocomp-sensitive-output-9d3f1a"

// TestTofuApplyCapturesInventory is the story-5 producer gate: a successful
// apply captures the workspace's declared, non-sensitive outputs into an
// inventory artifact, the sensitive-marked output is absent from it, and no
// event carries the sensitive value.
func TestTofuApplyCapturesInventory(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()

	// Declare one non-sensitive inventory output (host + group data) and one
	// sensitive output (a generated password) on top of the fixture's resource.
	appendOutputs(t, f.tofuDir)

	outPath := filepath.Join(t.TempDir(), "edge-inventory.json")
	drv := executor.NewTofu(executor.TofuOptions{
		Workspace:        f.workspace.Name,
		BackendURL:       api.StateURL(f.server.Addr(), f.workspace.Name),
		BackendToken:     f.token,
		StatePassphrase:  f.passphrase,
		PrivateDirRoot:   t.TempDir(),
		PluginCacheDir:   tofuPluginCache(t),
		OfflineProviders: true,
		InventoryOut:     outPath,
	})
	// The redactor knows only the envelope material, deliberately NOT the
	// sensitive output value: its absence from the log must be the capture
	// dropping it, not the redactor scrubbing it after the fact.
	q := queue.New(f.store, drv, queue.Options{Workers: 1, PollInterval: 20 * time.Millisecond},
		queue.WithRedactor(secrets.NewRedactor([][]byte{[]byte(f.token), []byte(f.passphrase)})))

	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook:        "workspace:" + f.workspace.Name,
		Kind:           executor.KindTofu,
		Trigger:        "test",
		Argv:           executor.TofuArgv("apply", "-auto-approve"),
		Dir:            f.tofuDir,
		ConcurrencyKey: "tofu:" + f.workspace.Name,
	})
	if err != nil {
		t.Fatalf("enqueue apply: %v", err)
	}
	f.server.BindJob(f.workspace.Name, job.ID)
	defer f.server.ReleaseJob(f.workspace.Name)
	if err := q.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	final, err := f.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if final.State != store.StateSucceeded {
		t.Fatalf("apply ended %s:\n%s", final.State, f.log(job.ID))
	}

	// The materialized inventory holds the non-sensitive host/group data...
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read materialized inventory: %v", err)
	}
	for _, want := range []string{"webnode", "web", "localhost"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("inventory missing %q:\n%s", want, body)
		}
	}
	// ...and NOT the sensitive value. This is the governing rule.
	if strings.Contains(string(body), sensitiveCanary) {
		t.Errorf("materialized inventory leaked the sensitive output:\n%s", body)
	}
	// It is a valid ansible JSON inventory.
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("materialized inventory is not valid JSON: %v\n%s", err, body)
	}

	// The capture event names what it kept and what it dropped — names only.
	capture := findCapture(t, f, job.ID)
	if !contains(capture.Captured, "ansible_inventory") {
		t.Errorf("capture did not record the inventory output: %+v", capture)
	}
	if !contains(capture.Suppressed, "db_password") {
		t.Fatalf("capture did not suppress the sensitive output: %+v", capture)
	}

	// No event anywhere carries the sensitive value.
	if log := f.log(job.ID); strings.Contains(log, sensitiveCanary) {
		t.Errorf("an event carried the sensitive output value:\n%s", log)
	}
}

// appendOutputs adds the story-5 outputs to the fixture's configuration: a
// non-sensitive ansible_inventory (host + group) and a sensitive password.
func appendOutputs(t *testing.T, tofuDir string) {
	t.Helper()
	block := `
output "db_password" {
  value     = "` + sensitiveCanary + `"
  sensitive = true
}
output "ansible_inventory" {
  value = {
    all = {
      hosts    = { localhost = { ansible_connection = "local" } }
      children = { web = { hosts = { webnode = {} } } }
    }
  }
}
`
	path := filepath.Join(tofuDir, "main.tf")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(path, append(existing, block...), 0o600); err != nil {
		t.Fatalf("append outputs: %v", err)
	}
}

// findCapture pulls the inventory-capture summary out of the job's events.
func findCapture(t *testing.T, f *fixture, jobID int64) executor.InventoryCapture {
	t.Helper()
	events, err := f.store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range events {
		if e.RawJSON == "" {
			continue
		}
		var env struct {
			Capture *executor.InventoryCapture `json:"tofu_inventory"`
		}
		if json.Unmarshal([]byte(e.RawJSON), &env) == nil && env.Capture != nil {
			return *env.Capture
		}
	}
	t.Fatalf("job %d recorded no inventory capture:\n%s", jobID, f.log(jobID))
	return executor.InventoryCapture{}
}

func contains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}
