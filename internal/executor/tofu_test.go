package executor_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// rawJSON returns the structured payloads the driver attached to events.
func (c *collector) rawJSON() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if e.RawJSON != "" {
			out = append(out, e.RawJSON)
		}
	}
	return out
}

// probe is a stand-in tofu: it records the argv and the environment it was
// given, then exits with the code the test chose for that operation.
type probe struct {
	binary  string
	logPath string
	dir     string
}

func newProbe(t *testing.T, exits map[string]int) *probe {
	t.Helper()

	root := t.TempDir()
	p := &probe{
		binary:  filepath.Join(root, executor.TofuBinary),
		logPath: filepath.Join(root, "invocations"),
		dir:     t.TempDir(),
	}

	var cases strings.Builder
	for op, code := range exits {
		fmt.Fprintf(&cases, "  %s) exit %d ;;\n", op, code)
	}

	script := fmt.Sprintf(`#!/bin/sh
{
  echo "argv: $*"
  echo "TF_DATA_DIR=${TF_DATA_DIR}"
  echo "TF_IN_AUTOMATION=${TF_IN_AUTOMATION}"
  echo "TF_ENCRYPTION_LEN=$(printf %%s "${TF_ENCRYPTION}" | wc -c)"
  echo "LEAK_CANARY=${EXOCOMP_LEAK_CANARY:-absent}"
  echo "OVERRIDE=$([ -f %[2]q ] && echo present || echo missing)"
} >> %[1]q
case "$1" in
%[3]s  *) exit 0 ;;
esac
`, p.logPath, filepath.Join(p.dir, executor.BackendOverrideFile), cases.String())

	if err := os.WriteFile(p.binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	return p
}

// lines returns what the probe recorded, one entry per line.
func (p *probe) lines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(p.logPath)
	if err != nil {
		t.Fatalf("read probe log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// argvs returns the argument lists the probe saw, in order.
func (p *probe) argvs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range p.lines(t) {
		if rest, ok := strings.CutPrefix(line, "argv: "); ok {
			out = append(out, rest)
		}
	}
	return out
}

func (p *probe) run(t *testing.T, opts executor.TofuOptions, argv []string) (executor.Result, *collector) {
	t.Helper()

	opts.Binary = p.binary
	if opts.BackendURL == "" {
		opts.BackendURL = "http://127.0.0.1:41111/api/v1/tf/state/edge"
	}
	if opts.PrivateDirRoot == "" {
		opts.PrivateDirRoot = t.TempDir()
	}

	sink := &collector{}
	res, err := executor.NewTofu(opts).Run(context.Background(), executor.Spec{
		JobID: 1,
		Kind:  executor.KindTofu,
		Argv:  argv,
		Dir:   p.dir,
	}, sink)
	if err != nil {
		t.Fatalf("run %v: %v", argv, err)
	}
	return res, sink
}

func TestTofuFlagsFollowTheCommandPath(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{
			// A positional plan file must stay last: tofu stops reading flags
			// at the first argument that is not one.
			name: "apply a saved plan",
			argv: executor.TofuArgv("apply", "/srv/plans/edge.tfplan"),
			want: "apply -no-color -input=false /srv/plans/edge.tfplan",
		},
		{
			// A subcommand must stay first, for the same reason in reverse.
			name: "state list",
			argv: executor.TofuArgv("state", "list"),
			want: "state list -no-color",
		},
		{
			name: "plan",
			argv: executor.TofuArgv("plan", "-out=/srv/plans/edge.tfplan"),
			want: "plan -no-color -input=false -detailed-exitcode -out=/srv/plans/edge.tfplan",
		},
		{
			name: "caller's own flags survive",
			argv: executor.TofuArgv("plan", "-no-color", "-input=true", "-detailed-exitcode"),
			want: "plan -no-color -input=true -detailed-exitcode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProbe(t, nil)
			p.run(t, executor.TofuOptions{}, tc.argv)

			argvs := p.argvs(t)
			if len(argvs) != 2 {
				t.Fatalf("probe saw %d invocations, want init then the operation: %v", len(argvs), argvs)
			}
			if !strings.HasPrefix(argvs[0], "init -no-color -input=false -backend-config=") {
				t.Errorf("init argv = %q, want the non-interactive flags and the private backend config",
					argvs[0])
			}
			if argvs[1] != tc.want {
				t.Errorf("operation argv = %q, want %q", argvs[1], tc.want)
			}
		})
	}
}

func TestTofuInitFailureStopsTheRun(t *testing.T) {
	p := newProbe(t, map[string]int{"init": 3})
	res, _ := p.run(t, executor.TofuOptions{}, executor.TofuArgv("apply", "-auto-approve"))

	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want init's 3", res.ExitCode)
	}
	if argvs := p.argvs(t); len(argvs) != 1 {
		t.Errorf("probe saw %v, want the operation never to have run", argvs)
	}
}

func TestTofuPlanReportsChangesWithoutFailing(t *testing.T) {
	cases := []struct {
		name     string
		planExit int
		changes  bool
	}{
		{"changes present", 2, true},
		{"no changes", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProbe(t, map[string]int{"plan": tc.planExit})
			res, sink := p.run(t, executor.TofuOptions{}, executor.TofuArgv("plan"))

			if res.ExitCode != 0 {
				t.Errorf("exit code = %d, want 0: a plan that found changes is not a failed job",
					res.ExitCode)
			}

			raws := sink.rawJSON()
			if len(raws) != 1 {
				t.Fatalf("plan outcomes = %v, want exactly one", raws)
			}
			changes, ok := executor.TofuPlanChanges(raws[0])
			if !ok {
				t.Fatalf("outcome %q is not a plan outcome", raws[0])
			}
			if changes != tc.changes {
				t.Errorf("changes = %v, want %v", changes, tc.changes)
			}
		})
	}
}

func TestTofuPlanFailureKeepsItsExitCode(t *testing.T) {
	p := newProbe(t, map[string]int{"plan": 1})
	res, sink := p.run(t, executor.TofuOptions{}, executor.TofuArgv("plan"))

	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}
	if raws := sink.rawJSON(); len(raws) != 0 {
		t.Errorf("plan outcomes = %v, want none for a broken plan", raws)
	}
}

func TestTofuKeepsSecretsOutOfArgvAndEnvironment(t *testing.T) {
	const token = "Ym9ndXMtdG9rZW4tZm9yLXRoZS10ZXN0LXN1aXRl"
	const passphrase = "cGFzc3BocmFzZS1mb3ItdGhlLXRlc3Qtc3VpdGU="
	t.Setenv("EXOCOMP_LEAK_CANARY", "leaked")

	p := newProbe(t, nil)
	res, _ := p.run(t, executor.TofuOptions{
		Workspace:       "edge",
		BackendToken:    token,
		StatePassphrase: passphrase,
	}, executor.TofuArgv("plan"))
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}

	lines := p.lines(t)
	joined := strings.Join(lines, "\n")
	for _, secret := range []string{token, passphrase} {
		for _, argv := range p.argvs(t) {
			if strings.Contains(argv, secret) {
				t.Errorf("argv %q carries a secret", argv)
			}
		}
	}
	if strings.Contains(joined, "LEAK_CANARY=leaked") {
		t.Error("the caller's unrelated environment reached the tofu process")
	}
	if !strings.Contains(joined, "TF_IN_AUTOMATION=1") {
		t.Error("TF_IN_AUTOMATION not set")
	}
	if strings.Contains(joined, "TF_ENCRYPTION_LEN=0") {
		t.Error("TF_ENCRYPTION not set, so state would be pushed in the clear")
	}
	if !strings.Contains(joined, "OVERRIDE=present") {
		t.Error("the backend override file was not in place while tofu ran")
	}
}

func TestTofuLeavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	p := newProbe(t, nil)
	p.run(t, executor.TofuOptions{PrivateDirRoot: root}, executor.TofuArgv("plan"))

	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read private root: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("private root holds %d entries, want none", len(left))
	}

	override := filepath.Join(p.dir, executor.BackendOverrideFile)
	if _, err := os.Stat(override); !os.IsNotExist(err) {
		t.Errorf("backend override still in the workspace directory: %v", err)
	}
}

func TestTofuRejectsBadSpec(t *testing.T) {
	p := newProbe(t, nil)
	drv := executor.NewTofu(executor.TofuOptions{
		Binary:         p.binary,
		BackendURL:     "http://127.0.0.1:41111/api/v1/tf/state/edge",
		PrivateDirRoot: t.TempDir(),
	})

	cases := []struct {
		name string
		spec executor.Spec
	}{
		{"wrong kind", executor.Spec{Kind: "ansible", Argv: executor.TofuArgv("plan"), Dir: p.dir}},
		{"no operation", executor.Spec{Kind: executor.KindTofu, Argv: []string{"tofu"}, Dir: p.dir}},
		{"foreign binary", executor.Spec{Kind: executor.KindTofu, Argv: []string{"terraform", "plan"}, Dir: p.dir}},
		{"flag for an operation", executor.Spec{Kind: executor.KindTofu, Argv: []string{"tofu", "-help"}, Dir: p.dir}},
		{"no working directory", executor.Spec{Kind: executor.KindTofu, Argv: executor.TofuArgv("plan")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := drv.Run(context.Background(), tc.spec, &collector{}); err == nil {
				t.Error("run accepted the spec")
			}
		})
	}
}

func TestTofuRejectsAnUnwritableBackendURL(t *testing.T) {
	p := newProbe(t, nil)
	drv := executor.NewTofu(executor.TofuOptions{
		Binary:         p.binary,
		BackendURL:     `http://127.0.0.1/${var.oops}`,
		PrivateDirRoot: t.TempDir(),
	})
	_, err := drv.Run(context.Background(), executor.Spec{
		Kind: executor.KindTofu,
		Argv: executor.TofuArgv("plan"),
		Dir:  p.dir,
	}, &collector{})
	if err == nil {
		t.Error("run accepted a backend url that would become HCL interpolation")
	}
}
