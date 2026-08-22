package executor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

const driftShowJSON = `{
  "format_version": "1.2",
  "resource_drift": [
    {
      "address": "proxmox_vm_qemu.web",
      "change": {
        "actions": ["update"],
        "before": {"id": "100", "cores": 2, "memory": 2048},
        "after":  {"id": "100", "cores": 4, "memory": 2048}
      }
    }
  ],
  "resource_changes": [
    {
      "address": "local_file.render",
      "change": {
        "actions": ["create"],
        "before": null,
        "after": {"filename": "/etc/app.conf", "content": "TOP-SECRET-VALUE"}
      }
    },
    {
      "address": "proxmox_vm_qemu.idle",
      "change": {"actions": ["no-op"], "before": {"id": "101"}, "after": {"id": "101"}}
    }
  ]
}`

func TestParsePlanShowJSON(t *testing.T) {
	t.Parallel()

	drift, pending, err := executor.ParsePlanShowJSON([]byte(driftShowJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(drift) != 1 || drift[0].Address != "proxmox_vm_qemu.web" {
		t.Fatalf("drift = %+v, want one proxmox_vm_qemu.web", drift)
	}
	if !slices.Equal(drift[0].Actions, []string{"update"}) {
		t.Fatalf("drift actions = %v", drift[0].Actions)
	}
	if !slices.Equal(drift[0].AttrKeys, []string{"cores", "id", "memory"}) {
		t.Fatalf("drift attr keys = %v, want sorted union of before/after", drift[0].AttrKeys)
	}

	// The no-op resource is filtered; only the real config change survives.
	if len(pending) != 1 || pending[0].Address != "local_file.render" {
		t.Fatalf("pending = %+v, want one local_file.render", pending)
	}
	if !slices.Equal(pending[0].AttrKeys, []string{"content", "filename"}) {
		t.Fatalf("pending attr keys = %v", pending[0].AttrKeys)
	}

	// Values are suppressed: no attribute value can appear anywhere in the
	// structured outcome, only the keys.
	blob, _ := json.Marshal(struct {
		Drift, Pending []executor.ResourceDelta
	}{drift, pending})
	for _, secret := range []string{"TOP-SECRET-VALUE", "2048", "/etc/app.conf"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("attribute VALUE %q leaked into the drift outcome: %s", secret, blob)
		}
	}
}

func TestParsePlanShowJSONRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, _, err := executor.ParsePlanShowJSON([]byte("not json")); err == nil {
		t.Fatal("parse of garbage should error")
	}
}

// fakeTofuWithShow writes a stand-in tofu: init succeeds, plan writes a plan
// file and exits 2 (changes present), and `show -json` prints the drift doc.
func fakeTofuWithShow(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, executor.TofuBinary)
	jsonPath := filepath.Join(dir, "show.json")
	if err := os.WriteFile(jsonPath, []byte(driftShowJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
op="$1"
case "$op" in
  init) exit 0 ;;
  show)
    cat %q
    exit 0 ;;
  plan)
    # find -out=PATH and write a plan file there
    for a in "$@"; do
      case "$a" in
        -out=*) echo "PLAN" > "${a#-out=}" ;;
      esac
    done
    exit 2 ;;
  *) exit 0 ;;
esac
`, jsonPath)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestTofuPlanReportsDrift is the story-3 must-fix at the driver level: a plan
// that finds drift attaches resource addresses and attribute keys to its event,
// distinct from a bare "changes" bool.
func TestTofuPlanReportsDrift(t *testing.T) {
	t.Parallel()
	bin := fakeTofuWithShow(t)
	dir := t.TempDir()
	planOut := filepath.Join(t.TempDir(), "drift.tfplan")

	sink := &collector{}
	res, err := executor.NewTofu(executor.TofuOptions{
		Binary:         bin,
		Workspace:      "edge",
		BackendURL:     "http://127.0.0.1:41111/api/v1/tf/state/edge",
		BackendToken:   "tok",
		PrivateDirRoot: t.TempDir(),
	}).Run(context.Background(), executor.Spec{
		JobID: 1,
		Kind:  executor.KindTofu,
		Argv:  executor.TofuArgv("plan", "-refresh-only", "-out="+planOut),
		Dir:   dir,
	}, sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Changes-present (exit 2) is not a failure.
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}

	var found *executor.TofuPlanOutcome
	for _, raw := range sink.rawJSON() {
		var env struct {
			Tofu *executor.TofuPlanOutcome `json:"tofu"`
		}
		if json.Unmarshal([]byte(raw), &env) == nil && env.Tofu != nil {
			found = env.Tofu
		}
	}
	if found == nil {
		t.Fatalf("no plan outcome emitted; events: %v", sink.rawJSON())
	}
	if !found.Changes {
		t.Fatal("outcome.Changes = false, want true")
	}
	if len(found.Drift) != 1 || found.Drift[0].Address != "proxmox_vm_qemu.web" {
		t.Fatalf("outcome.Drift = %+v, want the drifted address", found.Drift)
	}
	if !slices.Equal(found.Drift[0].AttrKeys, []string{"cores", "id", "memory"}) {
		t.Fatalf("outcome drift attr keys = %v", found.Drift[0].AttrKeys)
	}
	if len(found.Pending) != 1 || found.Pending[0].Address != "local_file.render" {
		t.Fatalf("outcome.Pending = %+v, want the config change separated out", found.Pending)
	}
}
