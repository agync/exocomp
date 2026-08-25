package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/verify"
)

// TestPlaybookRunHonorsSourceHooks proves item 9: `playbook run --source`
// composes the same lifecycle workflow `exocomp run` does, so a source's
// pre-run hook fires before the play no matter which command runs it.
func TestPlaybookRunHonorsSourceHooks(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	ansibleLog := fakeTool(t, executor.AnsibleBinary, "exit 0")

	runCLI(t, bin, "init")

	// A path source carrying a playbook and a pre_run hook that leaves a
	// marker, so the test can prove the hook ran and ran before the play.
	srcRoot := t.TempDir()
	writeFile(t, srcRoot+"/site.yml", "- hosts: all\n")
	if err := os.MkdirAll(srcRoot+"/.exocomp", 0o755); err != nil {
		t.Fatalf("mkdir .exocomp: %v", err)
	}
	prehook := t.TempDir() + "/prehook.ran"
	writeFile(t, srcRoot+"/.exocomp/hooks.yml",
		"pre_run:\n  - name: prep\n    run: \"echo ran > "+prehook+"\"\n")
	runCLI(t, bin, "source", "add", "infra", "--path", srcRoot)

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "disabled")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, code := runCLIExit(t, bin, "playbook", "run", "--source", "infra", "site.yml", "--wait")
	if code != 0 {
		t.Fatalf("playbook run exited %d:\n%s\n%s", code, out, d.dump())
	}
	if !strings.Contains(out, "succeeded") {
		t.Fatalf("run did not report success:\n%s\n%s", out, d.dump())
	}

	// The composed workflow is [pre-hook] -> play, both succeeded.
	workflowID := firstMatch(t, out, `workflow (run:\S+) started`)
	steps := workflowSteps(t, workflowID)
	if len(steps) != 2 {
		t.Fatalf("workflow has %d steps, want 2 (hook + play):\n%s", len(steps), d.dump())
	}
	if steps[0].Kind != hooks.JobKind || steps[1].Kind != executor.KindAnsible {
		t.Fatalf("step kinds = %q,%q, want %q,%q", steps[0].Kind, steps[1].Kind, hooks.JobKind, executor.KindAnsible)
	}
	for _, s := range steps {
		if s.Status != store.StepSucceeded {
			t.Errorf("step %d (%s) is %s, want succeeded\n%s", s.StepNo, s.Kind, s.Status, d.dump())
		}
	}

	// The hook actually ran, and the play ran after it.
	if got := readFile(t, prehook); !strings.Contains(got, "ran") {
		t.Errorf("pre_run hook left no marker; it did not run:\n%s", d.dump())
	}
	if readFile(t, ansibleLog) == "" {
		t.Errorf("the play did not run:\n%s", d.dump())
	}

	// The play step's job carries the source name, the fact the "running" event
	// and every job face read back rather than the checkout's absolute path.
	if steps[1].JobID == nil {
		t.Fatalf("play step has no job id:\n%s", d.dump())
	}
	playJob := getJob(t, *steps[1].JobID)
	if playJob.Args.Source != "infra" {
		t.Errorf("play job Args.Source = %q, want %q", playJob.Args.Source, "infra")
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// TestWrapWithHooksOrder proves the handoff's composition wraps its main steps
// in the source lifecycle in the right order: pre-hooks, the (multi-step) main
// chain, post-hooks, then the gate. It is the shared wrapper `handoff run` uses.
func TestWrapWithHooksOrder(t *testing.T) {
	main := []queue.Step{
		{Kind: "tofu-apply", Spec: queue.Spec{Kind: executor.KindTofu}},
		{Kind: "ansible-play", Spec: queue.Spec{Kind: executor.KindAnsible}},
	}
	hk := hooks.Hooks{
		PreRun:  []hooks.Hook{{Name: "prep", Run: "true"}},
		PostRun: []hooks.Hook{{Name: "notify", Run: "true"}},
		Gate:    []verify.Probe{{Name: "web", HTTP: &verify.HTTPProbe{URL: "http://127.0.0.1:9/"}}},
	}
	steps, err := wrapWithHooks("/src", true, hk, main, "infra", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{hooks.JobKind, "tofu-apply", "ansible-play", hooks.JobKind, verify.JobKind}
	if len(steps) != len(want) {
		t.Fatalf("wrapped into %d steps, want %d: %+v", len(steps), len(want), steps)
	}
	for i, k := range want {
		if steps[i].Kind != k {
			t.Fatalf("step %d kind = %q, want %q", i+1, steps[i].Kind, k)
		}
	}
	if !verify.IsGateArgv(steps[4].Spec.Argv) {
		t.Fatal("the final step is not the health gate")
	}
	// A read-only source marks its hook steps read-only too, so a hook cannot
	// write back to the checkout under the sandbox.
	if !steps[0].Spec.DirReadOnly || !steps[3].Spec.DirReadOnly {
		t.Fatalf("read-only source did not mark its hook steps read-only: %+v", steps)
	}
	// The hook and gate steps this wrapper adds carry the source name; the
	// caller's own main steps are untouched (they may run against something
	// other than this source's checkout, e.g. handoff's apply/play chain).
	if steps[0].Spec.Source != "infra" || steps[3].Spec.Source != "infra" || steps[4].Spec.Source != "infra" {
		t.Fatalf("wrapper steps did not carry the source name: %+v", steps)
	}
}

// TestWrapWithHooksEmptyIsUnchanged: a source with no hooks leaves the main
// chain exactly as it was — the "behaves exactly as today" guarantee.
func TestWrapWithHooksEmptyIsUnchanged(t *testing.T) {
	main := []queue.Step{{Kind: "tofu-apply"}, {Kind: "ansible-play"}}
	steps, err := wrapWithHooks("/src", false, hooks.Hooks{}, main, "infra", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].Kind != "tofu-apply" || steps[1].Kind != "ansible-play" {
		t.Fatalf("empty hooks changed the chain: %+v", steps)
	}
}
