package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/verify"
)

func TestComposeRunStepsOrder(t *testing.T) {
	hk := hooks.Hooks{
		PreRun:  []hooks.Hook{{Name: "prep", Run: "true"}},
		PostRun: []hooks.Hook{{Name: "notify", Script: "notify.sh"}},
		Gate:    []verify.Probe{{Name: "web", HTTP: &verify.HTTPProbe{URL: "http://127.0.0.1:9/"}}},
	}
	steps, err := composeRunSteps("/src", true, "/src/site.yml", "", "", runSpec{}, hk)
	if err != nil {
		t.Fatal(err)
	}

	// [pre-hook] -> play -> [post-hook] -> gate, in that exact order.
	wantKinds := []string{hooks.JobKind, executor.KindAnsible, hooks.JobKind, verify.JobKind}
	if len(steps) != len(wantKinds) {
		t.Fatalf("expected %d steps, got %d", len(wantKinds), len(steps))
	}
	for i, k := range wantKinds {
		if steps[i].Kind != k {
			t.Fatalf("step %d kind = %q, want %q", i+1, steps[i].Kind, k)
		}
	}
	// A read-only source marks its play and hook steps read-only; the gate step
	// has no working directory to grant.
	if !steps[0].Spec.DirReadOnly || !steps[1].Spec.DirReadOnly || !steps[2].Spec.DirReadOnly {
		t.Fatalf("read-only source did not propagate to the run steps: %+v", steps)
	}

	// The pre-hook is an inline shell command; the post-hook a script under the
	// checkout root.
	if got := strings.Join(steps[0].Spec.Argv, " "); got != hookShell+" -c true" {
		t.Fatalf("pre-hook argv = %q", got)
	}
	if got := strings.Join(steps[2].Spec.Argv, " "); got != hookShell+" /src/notify.sh" {
		t.Fatalf("post-hook argv = %q", got)
	}
	// The gate step carries the encoded probes, decodable back to the same set.
	if !verify.IsGateArgv(steps[3].Spec.Argv) {
		t.Fatal("gate step does not carry a gate argv")
	}
	probes, err := verify.Decode(steps[3].Spec.Argv)
	if err != nil || len(probes) != 1 || probes[0].Name != "web" {
		t.Fatalf("gate step did not round-trip: %v %+v", err, probes)
	}
}

func TestComposeRunStepsNoHooksIsJustPlay(t *testing.T) {
	steps, err := composeRunSteps("/src", false, "/src/site.yml", "", "", runSpec{}, hooks.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Kind != executor.KindAnsible {
		t.Fatalf("expected a single ansible step, got %d: %+v", len(steps), steps)
	}
}

func TestHookStepSecretBinding(t *testing.T) {
	// A hook's secret binding NAME travels on the spec; the value is resolved at
	// execution time, never stored on the row.
	step := hookStep("/src", false, hooks.Hook{Name: "auth", Run: "true", Secret: "proxmox"}, "", 0)
	if step.Spec.RunSecretBinding != "proxmox" {
		t.Fatalf("secret binding not carried: %+v", step.Spec)
	}
}

// TestComposeRunStepsCarriesSource proves a --source run's every step (hooks,
// play, gate) carries the source name onto its spec, so the "running" event
// and every job face can show it rather than the checkout path.
func TestComposeRunStepsCarriesSource(t *testing.T) {
	hk := hooks.Hooks{
		PreRun: []hooks.Hook{{Name: "prep", Run: "true"}},
		Gate:   []verify.Probe{{Name: "web", HTTP: &verify.HTTPProbe{URL: "http://127.0.0.1:9/"}}},
	}
	steps, err := composeRunSteps("/src", false, "/src/site.yml", "", "", runSpec{sourceName: "infra"}, hk)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range steps {
		if s.Spec.Source != "infra" {
			t.Errorf("step %d (%s) Source = %q, want %q", i, s.Kind, s.Spec.Source, "infra")
		}
	}
}

// collectSink records the events a driver emits, for the gate-driver tests.
type collectSink struct {
	mu    sync.Mutex
	lines []string
}

func (c *collectSink) Emit(e executor.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, e.Msg)
}

func (c *collectSink) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func TestVerifyDriverMapsProbeToExit(t *testing.T) {
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()

	argv, err := verify.Encode([]verify.Probe{{Name: "web", HTTP: &verify.HTTPProbe{URL: srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	drv := verifyJobDriver{}

	// A healthy endpoint -> exit 0 (the gate passes the workflow).
	sink := &collectSink{}
	res, err := drv.Run(context.Background(), executor.Spec{Kind: verify.JobKind, Argv: argv}, sink)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("expected exit 0 on a passing gate, got code=%d err=%v", res.ExitCode, err)
	}

	// The SAME gate against a 500 -> exit 1, and the failing probe is named in
	// the event stream (what makes "the gate names the failing probe" true).
	status = 500
	sink = &collectSink{}
	res, err = drv.Run(context.Background(), executor.Spec{Kind: verify.JobKind, Argv: argv}, sink)
	if err != nil {
		t.Fatalf("driver error on a failing gate: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("expected a non-zero exit when a gate probe fails")
	}
	if !strings.Contains(sink.joined(), "gate FAILED: web") {
		t.Fatalf("failing probe not named in events:\n%s", sink.joined())
	}
}

func TestVerifyDriverRefusesUndecodableGate(t *testing.T) {
	// A gate that cannot be decoded must error, never silently pass.
	drv := verifyJobDriver{}
	if _, err := drv.Run(context.Background(),
		executor.Spec{Kind: verify.JobKind, Argv: []string{"exocomp-gate", "@@@"}}, &collectSink{}); err == nil {
		t.Fatal("expected an undecodable gate to error")
	}
}
