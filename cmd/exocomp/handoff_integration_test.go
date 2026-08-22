package main

import (
	"encoding/base64"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// handoffSensitive is the value a sensitive-marked tofu output holds in these
// tests. It must never reach the materialized inventory or the play.
const handoffSensitive = "HANDOFF-SENSITIVE-7c1e"

// fakeTofuOutput is the `tofu output -json` a stand-in tofu prints: one
// non-sensitive ansible_inventory (host + group) and one sensitive password.
const fakeTofuOutput = `{"ansible_inventory":{"sensitive":false,"value":` +
	`{"all":{"hosts":{"localhost":{"ansible_connection":"local"}},` +
	`"children":{"web":{"hosts":{"webnode":{}}}}}}},` +
	`"db_password":{"sensitive":true,"value":"` + handoffSensitive + `"}}`

// TestHandoffChainRunsAndIsolates is the story-5 chain end to end: an apply
// captures the workspace's non-sensitive outputs into an inventory, and a
// dependent play consumes exactly that inventory — from its own private dir,
// with no bearer, no passphrase, and no tfstate in its environment.
func TestHandoffChainRunsAndIsolates(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	// Stand-in tofu: init/apply succeed, `output -json` prints the outputs. The
	// real capture-and-isolate proof runs against real tofu in the e2e gate.
	fakeTool(t, executor.TofuBinary, "case \"$1\" in output) printf '%s' '"+fakeTofuOutput+"' ;; esac\nexit 0")
	// Stand-in ansible-playbook: report the inventory it was handed and its own
	// environment, so the test can inspect both from the job events.
	fakeTool(t, executor.AnsibleBinary, `inv=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--inventory" ]; then inv="$a"; fi
  prev="$a"
done
echo "PLAY_INV_PATH=$inv"
if [ -n "$inv" ]; then echo "PLAY_INV_BEGIN"; cat "$inv"; echo "PLAY_INV_END"; fi
echo "PLAY_ENV_BEGIN"
env
echo "PLAY_ENV_END"
exit 0`)

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, code := runCLIExit(t, bin, "handoff", "run", "wsp", playbook, "--wait")
	if code != 0 {
		t.Fatalf("handoff run exited %d:\n%s\n%s", code, out, d.dump())
	}
	if !strings.Contains(out, "succeeded") {
		t.Fatalf("handoff did not report success:\n%s\n%s", out, d.dump())
	}

	workflowID := parseWorkflowID(t, out)
	steps := workflowSteps(t, workflowID)
	if len(steps) != 2 {
		t.Fatalf("workflow has %d steps, want 2", len(steps))
	}
	for _, s := range steps {
		if s.Status != store.StepSucceeded {
			t.Errorf("step %d (%s) is %s, want succeeded\n%s", s.StepNo, s.Kind, s.Status, d.dump())
		}
	}

	// The play's own job events carry the proof.
	playJob := *steps[1].JobID
	play := jobEventText(t, playJob)

	// The inventory the play was handed lived in the run's private dir, not the
	// data-dir artifact, and it carried the non-sensitive host/group data.
	invPath := firstMatch(t, play, `PLAY_INV_PATH=(\S+)`)
	if !strings.Contains(invPath, "/run/") && !strings.Contains(invPath, "exocomp-ansible-") {
		t.Errorf("play read the inventory from %q, not a private run dir", invPath)
	}
	for _, want := range []string{"webnode", "web", "localhost"} {
		if !strings.Contains(play, want) {
			t.Errorf("the play's inventory is missing %q:\n%s", want, play)
		}
	}

	// The governing rule: the sensitive output reached neither the inventory nor
	// the play.
	if strings.Contains(play, handoffSensitive) {
		t.Errorf("the sensitive tofu output reached the play:\n%s", play)
	}

	// The fleet-facing play carries no bearer, no state passphrase, no tfstate.
	env := playEnvBlock(t, play)
	token := workspaceToken(t, "wsp")
	passphrase := workspacePassphrase(t, "wsp")
	for _, forbidden := range []string{token, passphrase, "TF_ENCRYPTION", "TF_DATA_DIR", "Authorization"} {
		if strings.Contains(env, forbidden) {
			t.Errorf("the play's environment carried %q:\n%s", forbidden, env)
		}
	}

	// The apply job recorded the inventory as an artifact, and it is registered.
	assertInventoryArtifact(t, *steps[0].JobID)

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// TestHandoffBlocksPlayWhenApplyFails proves the dependency edge: a failed apply
// leaves the play blocked and the workflow failed, both readable from the
// ledger.
func TestHandoffBlocksPlayWhenApplyFails(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	// apply fails; the play must never run.
	fakeTool(t, executor.TofuBinary, "case \"$1\" in apply) exit 1 ;; esac\nexit 0")
	ranMarker := fakeTool(t, executor.AnsibleBinary, "exit 0")

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, _ := runCLIExit(t, bin, "handoff", "run", "wsp", playbook, "--wait")
	workflowID := parseWorkflowID(t, out)
	steps := workflowSteps(t, workflowID)
	if steps[0].Status != store.StepFailed {
		t.Errorf("apply step is %s, want failed\n%s", steps[0].Status, d.dump())
	}
	if steps[1].Status != store.StepBlocked {
		t.Errorf("play step is %s, want blocked\n%s", steps[1].Status, d.dump())
	}
	if store.WorkflowStatus(steps) != store.WorkflowFailed {
		t.Errorf("workflow is %s, want failed", store.WorkflowStatus(steps))
	}
	if steps[1].JobID != nil && jobState(t, *steps[1].JobID) == store.StateSucceeded {
		t.Error("the play job ran despite the failed apply")
	}
	// The play binary was never invoked.
	if inv := readFile(t, ranMarker); inv != "" {
		t.Errorf("ansible-playbook ran despite the blocked step:\n%s", inv)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
}

// TestHandoffResumesAfterDaemonRestart proves the chain is crash-resumable: a
// daemon killed after the apply succeeded but before the play finished,
// restarted, continues the workflow from the ledger — the apply is not re-run,
// the play runs against the captured inventory, and the chain completes.
func TestHandoffResumesAfterDaemonRestart(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	gate := t.TempDir() + "/gate"
	applyLog := fakeTool(t, executor.TofuBinary,
		"case \"$1\" in output) printf '%s' '"+fakeTofuOutput+"' ;; esac\nexit 0")
	// The play blocks until the gate file appears, so the kill lands with step 1
	// (apply) already succeeded and step 2 (play) not yet done.
	playRan := fakeTool(t, executor.AnsibleBinary,
		"i=0; while [ ! -f "+gate+" ] && [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done; echo played; exit 0")

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d1 := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d1.waitForLine(t, &d1.stdout, "scheduler started", 30*time.Second)

	out, code := runCLIExit(t, bin, "handoff", "run", "wsp", playbook)
	if code != 0 {
		t.Fatalf("handoff run exited %d:\n%s", code, out)
	}
	workflowID := parseWorkflowID(t, out)

	// Wait until the apply has succeeded and the play has started (blocked on the
	// gate), then kill the daemon.
	waitFor(t, "the apply to succeed and the play to start", 30*time.Second, func() bool {
		steps := workflowSteps(t, workflowID)
		return steps[0].Status == store.StepSucceeded && steps[1].Status == store.StepRunning
	})
	applyRunsBefore := strings.Count(readFile(t, applyLog), "apply ")
	if err := d1.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill daemon: %v", err)
	}
	<-d1.done
	d1.exited = true

	// Let the play through once it is restarted.
	writeFile(t, gate, "go\n")

	d2 := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d2.waitForLine(t, &d2.stdout, "scheduler started", 30*time.Second)

	waitFor(t, "the workflow to finish after the restart", 60*time.Second, func() bool {
		return store.WorkflowStatus(workflowSteps(t, workflowID)) != store.WorkflowRunning
	})
	steps := workflowSteps(t, workflowID)
	if store.WorkflowStatus(steps) != store.WorkflowSucceeded {
		t.Errorf("resumed workflow is %s, want succeeded\n%s", store.WorkflowStatus(steps), d2.dump())
	}
	// The apply was not re-run across the restart: its success is durable in the
	// ledger.
	if after := strings.Count(readFile(t, applyLog), "apply "); after != applyRunsBefore {
		t.Errorf("apply ran %d times, want %d: a succeeded step was re-run", after, applyRunsBefore)
	}
	// The interrupted play WAS re-armed and re-run: the killed invocation plus
	// the resumed one. The play's success is what carried the workflow to
	// succeeded above; this proves the resume re-ran it rather than a stale
	// process finishing.
	if runs := strings.Count(readFile(t, playRan), "site.yml"); runs < 2 {
		t.Errorf("the play ran %d times, want the killed one re-armed and re-run", runs)
	}

	d2.signal(t, syscall.SIGTERM)
	d2.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d2)
}

// workflowSteps reads a workflow's steps from the ledger.
func workflowSteps(t *testing.T, workflowID string) []store.WorkflowStep {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	steps, err := st.GetWorkflow(t.Context(), workflowID)
	if err != nil {
		t.Fatalf("get workflow %s: %v", workflowID, err)
	}
	return steps
}

// assertInventoryArtifact checks the apply job recorded an inventory artifact
// and that it holds the non-sensitive data and not the sensitive value.
func assertInventoryArtifact(t *testing.T, applyJob int64) {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	arts, err := st.ListArtifacts(t.Context(), applyJob)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	var inv *store.Artifact
	for i := range arts {
		if arts[i].Kind == inventoryArtifactKind {
			inv = &arts[i]
		}
	}
	if inv == nil {
		t.Fatalf("apply job %d recorded no inventory artifact", applyJob)
	}
	body := readFile(t, inv.Path)
	if !strings.Contains(body, "webnode") {
		t.Errorf("inventory artifact missing host data:\n%s", body)
	}
	if strings.Contains(body, handoffSensitive) {
		t.Errorf("inventory artifact leaked the sensitive output:\n%s", body)
	}
}

func parseWorkflowID(t *testing.T, out string) string {
	t.Helper()
	return firstMatch(t, out, `workflow (handoff:\S+) started`)
}

func firstMatch(t *testing.T, s, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if len(m) < 2 {
		t.Fatalf("no match for %q in:\n%s", pattern, s)
	}
	return m[1]
}

// playEnvBlock extracts the env dump the stand-in play printed.
func playEnvBlock(t *testing.T, play string) string {
	t.Helper()
	start := strings.Index(play, "PLAY_ENV_BEGIN")
	end := strings.Index(play, "PLAY_ENV_END")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("no play env block in:\n%s", play)
	}
	return play[start:end]
}

func workspaceToken(t *testing.T, ws string) string {
	t.Helper()
	env := openEnvelope(t)
	defer func() { _ = env.Close() }()
	return api.WorkspaceToken(env, ws)
}

func workspacePassphrase(t *testing.T, ws string) string {
	t.Helper()
	env := openEnvelope(t)
	defer func() { _ = env.Close() }()
	return base64.StdEncoding.EncodeToString(env.Derive(executor.TofuStatePurpose, ws))
}

func openEnvelope(t *testing.T) *secrets.Envelope {
	t.Helper()
	dir, err := dataDir()
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	return env
}
