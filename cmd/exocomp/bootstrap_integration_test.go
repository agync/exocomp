package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// TestBootstrapComposesAnsibleFirst is the ordering unit: the composed step list
// is the handoff reversed — the ansible play first, the tofu apply second — and
// the apply carries the destructive shape (auto-approve, workspace concurrency
// key, the --run-secrets binding).
func TestBootstrapComposesAnsibleFirst(t *testing.T) {
	steps := composeBootstrapSteps("bootstrap:wsp:1", "/abs/site.yml", "/abs/inv", "tf:wsp", "/abs/tofu", "wsp",
		bootstrapSpec{runSecrets: "kube"})

	if len(steps) != 2 {
		t.Fatalf("composed %d steps, want 2", len(steps))
	}
	if got := stepKinds(steps); got[0] != "ansible-play" || got[1] != "tofu-apply" {
		t.Fatalf("step kinds %v, want [ansible-play tofu-apply]", got)
	}
	if steps[0].Spec.Kind != executor.KindAnsible {
		t.Errorf("step 1 spec kind %q, want %q", steps[0].Spec.Kind, executor.KindAnsible)
	}
	if steps[1].Spec.Kind != executor.KindTofu {
		t.Errorf("step 2 spec kind %q, want %q", steps[1].Spec.Kind, executor.KindTofu)
	}
	// The apply is the destructive leg: auto-approve, gated on the workspace's
	// tofu concurrency key, and the --run-secrets binding lands on it (Phase 0
	// captures nothing from the play, so the apply reads its own credential).
	if !strings.Contains(strings.Join(steps[1].Spec.Argv, " "), "apply -auto-approve") {
		t.Errorf("apply argv %v is not an auto-approve apply", steps[1].Spec.Argv)
	}
	if steps[1].Spec.RunSecretBinding != "kube" {
		t.Errorf("--run-secrets bound to %q, want the apply step", steps[1].Spec.RunSecretBinding)
	}
	if steps[0].Spec.RunSecretBinding != "" {
		t.Errorf("the play step carried a run-secret binding %q, want none in Phase 0",
			steps[0].Spec.RunSecretBinding)
	}
	// No --capture: neither step carries capture wiring.
	if steps[0].Spec.Captures != nil {
		t.Errorf("the play step carried captures without --capture: %+v", steps[0].Spec.Captures)
	}
	if steps[1].Spec.Captures != nil {
		t.Errorf("the apply step carried captures without --capture: %+v", steps[1].Spec.Captures)
	}
}

// TestBootstrapComposesCaptureBinding proves --capture rewires the chain: the
// play step carries the declared captures and the workflow id on its first-class
// Captures field (round-tripping through the driver's reader), the play's own
// extra-vars stay empty, and the apply step binds the same ab:<workflow> the play
// seals into — the closed env-decomposition loop, with no value anywhere.
func TestBootstrapComposesCaptureBinding(t *testing.T) {
	const wf = "bootstrap:wsp:42"
	captures := []executor.AnsibleCapture{{Name: "KUBECONFIG", File: "kubeconfig"}}
	steps := composeBootstrapSteps(wf, "/abs/site.yml", "/abs/inv", "tf:wsp", "/abs/tofu", "wsp",
		bootstrapSpec{captures: captures})

	// The apply inherits the chain's own ab:<workflow>, not a pre-existing binding.
	want := executor.AnsibleOutputBinding(wf)
	if steps[1].Spec.RunSecretBinding != want {
		t.Errorf("apply binding %q, want %q", steps[1].Spec.RunSecretBinding, want)
	}

	// The play's Captures field reads back to exactly the declared captures and
	// workflow, and the play's own extra-vars carry none of this wiring.
	if len(steps[0].Spec.ExtraVars) != 0 {
		t.Errorf("play extra-vars carried capture wiring: %v", steps[0].Spec.ExtraVars)
	}
	got, gotWF := bootstrapCaptureTargets(steps[0].Spec.Captures)
	if gotWF != wf {
		t.Errorf("captures workflow %q, want %q", gotWF, wf)
	}
	if len(got) != 1 || got[0] != captures[0] {
		t.Errorf("captures %v, want %v", got, captures)
	}
}

// TestParseCapturesRejectsBadSpecs proves the flag parser refuses a spec that is
// not NAME=FILE and a FILE that could escape the capture dir.
func TestParseCapturesRejectsBadSpecs(t *testing.T) {
	for _, bad := range []string{"KUBECONFIG", "=kubeconfig", "KUBECONFIG=", "K=/etc/passwd", "K=../escape"} {
		if _, err := parseCaptures([]string{bad}, false); err == nil {
			t.Errorf("parseCaptures(%q) accepted a bad spec", bad)
		}
	}
	got, err := parseCaptures([]string{"KUBECONFIG=kubeconfig", "TOKEN=sub/dir/token"}, false)
	if err != nil {
		t.Fatalf("parseCaptures rejected valid specs: %v", err)
	}
	if len(got) != 2 || got[0].Name != "KUBECONFIG" || got[1].File != "sub/dir/token" {
		t.Errorf("parseCaptures returned %v", got)
	}
	if got[0].AsFile {
		t.Errorf("--capture marked a capture as file delivery")
	}
	// --capture-file parses the same NAME=FILE, marking file delivery.
	file, err := parseCaptures([]string{"KUBECONFIG=kubeconfig"}, true)
	if err != nil {
		t.Fatalf("parseCaptures(asFile) rejected a valid spec: %v", err)
	}
	if len(file) != 1 || !file[0].AsFile {
		t.Errorf("--capture-file did not mark file delivery: %v", file)
	}
}

// TestBootstrapComposesFileStaging proves --capture-file marks the env name for
// file staging on the apply, derived from the apply step's Captures field, and an
// env-only --capture stages nothing. The value never appears: only the env NAME.
func TestBootstrapComposesFileStaging(t *testing.T) {
	const wf = "bootstrap:wsp:7"

	fileSteps := composeBootstrapSteps(wf, "/abs/site.yml", "/abs/inv", "tf:wsp", "/abs/tofu", "wsp",
		bootstrapSpec{captures: []executor.AnsibleCapture{{Name: "KUBECONFIG", File: "kubeconfig", AsFile: true}}})
	if envs := bootstrapStagedFileEnvs(fileSteps[1].Spec.Captures); len(envs) != 1 || envs[0] != "KUBECONFIG" {
		t.Errorf("apply stage-file envs %v, want [KUBECONFIG]", envs)
	}

	// An env-only capture stages no file, so every bound value delivers as env
	// (the Phase-2 shape).
	envSteps := composeBootstrapSteps(wf, "/abs/site.yml", "/abs/inv", "tf:wsp", "/abs/tofu", "wsp",
		bootstrapSpec{captures: []executor.AnsibleCapture{{Name: "KUBECONFIG", File: "kubeconfig"}}})
	if envs := bootstrapStagedFileEnvs(envSteps[1].Spec.Captures); len(envs) != 0 {
		t.Errorf("env-only capture staged files %v, want none", envs)
	}
}

// TestBootstrapChainRunsAnsibleThenTofu is the chain end to end: the bootstrap
// play runs first, the apply runs after it succeeded, and the ledger records the
// two steps ansible-first, both succeeded.
func TestBootstrapChainRunsAnsibleThenTofu(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	// A play that leaves a marker, then a tofu whose apply asserts the marker is
	// already there: the apply exits non-zero if it ran before the play, so the
	// ordering is proven by the chain succeeding, not just by the ledger order.
	gate := t.TempDir() + "/played"
	fakeTool(t, executor.AnsibleBinary, "echo BOOTSTRAP_PLAY_RAN; touch "+gate+"; exit 0")
	fakeTool(t, executor.TofuBinary,
		"case \"$1\" in output) printf '{}' ;; apply) [ -f "+gate+" ] || { echo 'apply ran before the play' >&2; exit 3; } ;; esac\nexit 0")

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, code := runCLIExit(t, bin, "bootstrap", "run", playbook, "wsp", "--wait")
	if code != 0 {
		t.Fatalf("bootstrap run exited %d:\n%s\n%s", code, out, d.dump())
	}
	if !strings.Contains(out, "succeeded") {
		t.Fatalf("bootstrap did not report success:\n%s\n%s", out, d.dump())
	}

	workflowID := parseBootstrapWorkflowID(t, out)
	steps := workflowSteps(t, workflowID)
	if len(steps) != 2 {
		t.Fatalf("workflow has %d steps, want 2", len(steps))
	}
	if steps[0].Kind != "ansible-play" || steps[1].Kind != "tofu-apply" {
		t.Fatalf("ledger kinds [%s %s], want [ansible-play tofu-apply]", steps[0].Kind, steps[1].Kind)
	}
	for _, s := range steps {
		if s.Status != store.StepSucceeded {
			t.Errorf("step %d (%s) is %s, want succeeded\n%s", s.StepNo, s.Kind, s.Status, d.dump())
		}
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// TestBootstrapBlocksApplyWhenPlayFails proves the dependency edge and the
// order: a failed bootstrap play leaves the apply blocked and the workflow
// failed, and the apply binary never ran. This is the ordering guarantee — the
// apply cannot precede the play.
func TestBootstrapBlocksApplyWhenPlayFails(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	fakeTool(t, executor.AnsibleBinary, "exit 1") // the play fails
	applyMarker := fakeTool(t, executor.TofuBinary, "exit 0")

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, _ := runCLIExit(t, bin, "bootstrap", "run", playbook, "wsp", "--wait")
	workflowID := parseBootstrapWorkflowID(t, out)
	steps := workflowSteps(t, workflowID)
	if steps[0].Status != store.StepFailed {
		t.Errorf("play step is %s, want failed\n%s", steps[0].Status, d.dump())
	}
	if steps[1].Status != store.StepBlocked {
		t.Errorf("apply step is %s, want blocked\n%s", steps[1].Status, d.dump())
	}
	if store.WorkflowStatus(steps) != store.WorkflowFailed {
		t.Errorf("workflow is %s, want failed", store.WorkflowStatus(steps))
	}
	// The apply binary was never invoked: no apply can precede a failed play.
	if inv := readFile(t, applyMarker); strings.Contains(inv, "apply") {
		t.Errorf("tofu apply ran despite the blocked step:\n%s", inv)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
}

// TestBootstrapOutputJSON proves --output json emits the wire envelope carrying
// the workflow id and the ordered, ansible-first step kinds.
func TestBootstrapOutputJSON(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	fakeTool(t, executor.AnsibleBinary, "exit 0")
	fakeTool(t, executor.TofuBinary, "case \"$1\" in output) printf '{}' ;; esac\nexit 0")

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, code := runCLIExit(t, bin, "bootstrap", "run", playbook, "wsp", "--output", "json")
	if code != 0 {
		t.Fatalf("bootstrap run --output json exited %d:\n%s\n%s", code, out, d.dump())
	}

	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Kind          string `json:"kind"`
		Data          struct {
			WorkflowID string   `json:"workflow_id"`
			Workspace  string   `json:"workspace"`
			Steps      []string `json:"steps"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", out, err)
	}
	if env.SchemaVersion != wire.SchemaVersion {
		t.Errorf("schema_version %d, want %d", env.SchemaVersion, wire.SchemaVersion)
	}
	if env.Kind != bootstrapWorkflowKind {
		t.Errorf("kind %q, want %q", env.Kind, bootstrapWorkflowKind)
	}
	if env.Data.Workspace != "wsp" {
		t.Errorf("workspace %q, want wsp", env.Data.Workspace)
	}
	if len(env.Data.Steps) != 2 || env.Data.Steps[0] != "ansible-play" || env.Data.Steps[1] != "tofu-apply" {
		t.Errorf("steps %v, want [ansible-play tofu-apply]", env.Data.Steps)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// TestBootstrapRequireApprovalGates proves --require-approval refuses to start
// the destructive chain without an explicit --assume-yes, and that --assume-yes
// opens the gate.
func TestBootstrapRequireApprovalGates(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	fakeTool(t, executor.AnsibleBinary, "exit 0")
	fakeTool(t, executor.TofuBinary, "case \"$1\" in output) printf '{}' ;; esac\nexit 0")

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	// No --assume-yes, non-interactive stdin: the gate refuses and nothing is
	// enqueued. No daemon is needed — the refusal is at the CLI, before any store
	// write.
	out, code := runCLIExit(t, bin, "bootstrap", "run", playbook, "wsp", "--require-approval")
	if code == 0 {
		t.Fatalf("bootstrap run --require-approval succeeded without confirmation:\n%s", out)
	}
	if !strings.Contains(out, "--assume-yes") {
		t.Errorf("refusal did not name --assume-yes:\n%s", out)
	}

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	// --assume-yes opens the gate: the chain composes and runs to success.
	out, code = runCLIExit(t, bin, "bootstrap", "run", playbook, "wsp", "--require-approval", "--assume-yes", "--wait")
	if code != 0 {
		t.Fatalf("bootstrap run --require-approval --assume-yes exited %d:\n%s\n%s", code, out, d.dump())
	}
	if !strings.Contains(out, "succeeded") {
		t.Fatalf("gated chain did not run to success:\n%s\n%s", out, d.dump())
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

func parseBootstrapWorkflowID(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`workflow (bootstrap:\S+) started`).FindStringSubmatch(out)
	if len(m) < 2 {
		t.Fatalf("no bootstrap workflow id in:\n%s", out)
	}
	return m[1]
}
