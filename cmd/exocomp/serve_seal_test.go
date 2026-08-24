package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

// serveSealSentinel stands in for the value a `sensitive = true` tofu output
// carries when the apply runs through the daemon rather than a CLI-inline
// apply. It may reach the play's environment and nothing else.
const serveSealSentinel = "SERVE-SEAL-SENTINEL-9c3e0b"

// serveSealOutputs is the `tofu output -json` the stand-in prints: a
// host-shaped inventory and one sensitive output.
const serveSealOutputs = `{"ansible_inventory":{"sensitive":false,"value":` +
	`{"all":{"hosts":{"localhost":{"ansible_connection":"local"}}}}},` +
	`"db_password":{"sensitive":true,"value":"` + serveSealSentinel + `"}}`

// TestDaemonApplySealsOutputIntoDependentPlay is the daemon half of item D:
// cmd/exocomp/serve.go's workspaceDriver.Run must seal and bind a sensitive
// tofu output exactly as the daemonless `workspace apply` path does, so a
// scheduled/reactive/provision-chain/serve-up apply leaves the tf:<workspace>
// binding populated for the dependent play. Without OutputSealer wired into
// the daemon's TofuOptions, run-secret list stays empty after this apply.
func TestDaemonApplySealsOutputIntoDependentPlay(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	fakeTool(t, executor.TofuBinary,
		"case \"$1\" in output) printf '%s' '"+serveSealOutputs+"' ;; esac\nexit 0")
	// Stand-in ansible-playbook: report the bound secret by LENGTH, never by
	// value, which is the only report a test of a secret may print.
	fakeTool(t, executor.AnsibleBinary, `echo "DBPWLEN=${#DB_PASSWORD}"
exit 0`)

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, filepath.Join(module, "main.tf"), "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := filepath.Join(module, "site.yml")
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	// A daemon is up: this apply is enqueued and run by the daemon's
	// workspaceDriver, not a CLI-inline driver.
	applyOut, applyCode := runCLIExit(t, bin, "workspace", "apply", "wsp")
	assertDaemonRan(t, applyOut, "workspace apply")
	if applyCode != 0 || !strings.Contains(applyOut, "state: succeeded") {
		t.Fatalf("workspace apply exited %d:\n%s", applyCode, applyOut)
	}
	assertNoSentinel(t, applyOut, "the apply's own output")

	// The binding the daemon's apply must have sealed: the bug this test
	// closes was exactly this row staying absent.
	listing, code := runCLIExit(t, bin, "run-secret", "list", "tf:wsp")
	if code != 0 {
		t.Fatalf("run-secret list exited %d:\n%s", code, listing)
	}
	if !strings.Contains(listing, "DB_PASSWORD") ||
		!strings.Contains(listing, "tf-output/wsp/db_password") {
		t.Fatalf("daemon apply did not bind tf:wsp to the sealed output:\n%s", listing)
	}

	// The dependent play, run through the same daemon, receives the value as
	// $DB_PASSWORD by naming only the workspace's standing inventory.
	play, code := runCLIExit(t, bin, "playbook", "run", playbook, "--inventory", "tf:wsp")
	assertDaemonRan(t, play, "playbook run")
	if code != 0 {
		t.Fatalf("playbook run exited %d:\n%s", code, play)
	}
	if want := "DBPWLEN=" + strconv.Itoa(len(serveSealSentinel)); !strings.Contains(play, want) {
		t.Errorf("the play did not receive $DB_PASSWORD (%s):\n%s", want, play)
	}
	assertNoSentinel(t, play, "playbook run")

	// Neither the recorded argv nor the durable event ledger of either job
	// carries the sealed value.
	assertNoSentinel(t, jobEventText(t, lastJobID(t, workspaceRunbookPrefix+"wsp")), "the apply job's events")
	assertNoSentinel(t, jobEventText(t, lastJobID(t, "playbook")), "the play job's events")

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}
