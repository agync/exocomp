package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// cliSink records every event a run emits, which is what the run-secret test
// scans for a leak.
type cliSink struct {
	mu   sync.Mutex
	msgs []string
}

func (s *cliSink) Emit(e executor.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, e.Msg)
	if e.RawJSON != "" {
		s.msgs = append(s.msgs, e.RawJSON)
	}
}

func (s *cliSink) joined() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.msgs, "\n")
}

// fakeTool writes an executable shell script and returns its path. The script
// is the stand-in for a registered vendor CLI: it is exec'd directly, never
// through a shell of this project's making, so whatever it does with its own
// arguments is its business and not an interpolation this package performed.
func fakeTool(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
	return path
}

// TestCLIExitCodeDecidesTheRun: the whole point of running a vendor CLI under
// the queue is that its own exit code — not a parse of its output — is the
// verdict (AGENTS.md invariant 3). Both directions are pinned.
func TestCLIExitCodeDecidesTheRun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"success", "echo ok\nexit 0\n", 0},
		{"failure", "echo boom >&2\nexit 7\n", 7},
	} {
		bin := fakeTool(t, "vendorctl", tc.body)
		drv := executor.NewCLI(executor.CLIOptions{Tool: "vendorctl", Binary: bin})

		sink := &cliSink{}
		res, err := drv.Run(context.Background(), executor.Spec{
			Kind: executor.KindCLI,
			Argv: executor.CLIArgv("vendorctl", "status"),
		}, sink)
		if err != nil {
			t.Fatalf("%s: driver error: %v", tc.name, err)
		}
		if res.ExitCode != tc.want {
			t.Errorf("%s: exit code = %d, want %d (%s)", tc.name, res.ExitCode, tc.want, sink.joined())
		}
		if res.Degraded {
			t.Errorf("%s: a CLI run must never report a degraded outcome", tc.name)
		}
	}
}

// TestCLIArgvIsNeverShellInterpolated is the injection property. A single
// argument carrying every metacharacter a shell would act on has to arrive at
// the process as ONE literal argv element: no word splitting, no globbing, no
// command substitution, no second command.
func TestCLIArgvIsNeverShellInterpolated(t *testing.T) {
	t.Parallel()

	// The tool prints its arguments one per line, prefixed, so both the count
	// and the exact bytes of each are observable.
	bin := fakeTool(t, "vendorctl", "for a in \"$@\"; do echo \"arg:$a\"; done\n")
	nasty := `; rm -rf / && $(id) | tee /tmp/x > *.txt ` + "`whoami`" + ` "quoted" 'single'`

	drv := executor.NewCLI(executor.CLIOptions{Tool: "vendorctl", Binary: bin})
	sink := &cliSink{}
	res, err := drv.Run(context.Background(), executor.Spec{
		Kind: executor.KindCLI,
		Argv: executor.CLIArgv("vendorctl", "apply", nasty),
	}, sink)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("run: err=%v exit=%d (%s)", err, res.ExitCode, sink.joined())
	}

	var args []string
	for _, line := range strings.Split(sink.joined(), "\n") {
		if rest, ok := strings.CutPrefix(line, "arg:"); ok {
			args = append(args, rest)
		}
	}
	if len(args) != 2 {
		t.Fatalf("tool saw %d arguments, want 2 — the metacharacter argument was split: %q", len(args), args)
	}
	if args[0] != "apply" {
		t.Errorf("arg[0] = %q, want %q", args[0], "apply")
	}
	if args[1] != nasty {
		t.Errorf("arg[1] = %q\nwant     %q", args[1], nasty)
	}
	// Command substitution would have replaced the text with its output; a glob
	// would have expanded it. The literal markers surviving prove neither ran.
	if !strings.Contains(args[1], "$(id)") || !strings.Contains(args[1], "*.txt") {
		t.Errorf("argument was interpolated somewhere: %q", args[1])
	}
}

// TestCLIRunSecretsInjectAsEnvNeverArgv seeds a sentinel through the
// credential plane and proves it reaches the process's environment while
// appearing in neither the argv nor a single job event (AGENTS.md invariant 4).
func TestCLIRunSecretsInjectAsEnvNeverArgv(t *testing.T) {
	t.Parallel()

	const sentinel = "exocomp-sentinel-4f2a9d7c-not-a-real-token"
	// The tool reports only the LENGTH of what it was handed, so a passing test
	// cannot itself be the leak: the value proves it arrived without printing.
	bin := fakeTool(t, "vendorctl", `printf 'token len %s\n' "${#VENDOR_TOKEN}"`+"\n")

	drv := executor.NewCLI(executor.CLIOptions{
		Tool:       "vendorctl",
		Binary:     bin,
		RunSecrets: executor.RunSecrets{"VENDOR_TOKEN": sentinel},
	})
	argv := executor.CLIArgv("vendorctl", "deploy", "--app", "site")
	sink := &cliSink{}
	res, err := drv.Run(context.Background(), executor.Spec{Kind: executor.KindCLI, Argv: argv}, sink)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("run: err=%v exit=%d (%s)", err, res.ExitCode, sink.joined())
	}

	events := sink.joined()
	if !strings.Contains(events, "token len "+strconv.Itoa(len(sentinel))) {
		t.Fatalf("the bound secret did not reach the process environment:\n%s", events)
	}
	if strings.Contains(events, sentinel) {
		t.Fatal("the run secret's value appeared in a job event")
	}
	for _, a := range argv {
		if strings.Contains(a, sentinel) {
			t.Fatalf("the run secret's value appeared in argv: %q", a)
		}
	}
}

// TestCLIRefusesARowItWasNotBuiltFor is the driver's half of the allow-list.
// The CLI refuses an unregistered tool before enqueue; if a row for one still
// reached a worker, the driver must not run it.
func TestCLIRefusesARowItWasNotBuiltFor(t *testing.T) {
	t.Parallel()

	bin := fakeTool(t, "vendorctl", "exit 0\n")
	drv := executor.NewCLI(executor.CLIOptions{Tool: "vendorctl", Binary: bin})

	cases := map[string]executor.Spec{
		"another tool's argv": {Kind: executor.KindCLI, Argv: executor.CLIArgv("curl", "https://example.invalid")},
		"a foreign kind":      {Kind: executor.KindAnsible, Argv: executor.CLIArgv("vendorctl", "status")},
		"an empty argv":       {Kind: executor.KindCLI},
		"a control character": {Kind: executor.KindCLI, Argv: executor.CLIArgv("vendorctl", "a\x00b")},
	}
	for name, spec := range cases {
		if _, err := drv.Run(context.Background(), spec, &cliSink{}); err == nil {
			t.Errorf("%s: the driver ran it, want a refusal", name)
		}
	}

	// A driver built with no tool at all refuses everything: an empty option is
	// a misconfiguration, not a wildcard.
	bare := executor.NewCLI(executor.CLIOptions{})
	if _, err := bare.Run(context.Background(), executor.Spec{
		Kind: executor.KindCLI, Argv: executor.CLIArgv("vendorctl"),
	}, &cliSink{}); err == nil {
		t.Error("a driver with no allow-listed tool ran a job")
	}
}

// TestCLIEnvironmentIsAllowlisted: a governed run must not inherit whatever the
// operator exported, or a credential the sandbox allowlist exists to strip
// would arrive through the back door.
func TestCLIEnvironmentIsAllowlisted(t *testing.T) {
	t.Setenv("EXOCOMP_TEST_LEAKED", "leaked-value")
	bin := fakeTool(t, "vendorctl", `echo "leak=${EXOCOMP_TEST_LEAKED-unset} path=${PATH:+set}"`+"\n")

	drv := executor.NewCLI(executor.CLIOptions{Tool: "vendorctl", Binary: bin})
	sink := &cliSink{}
	if _, err := drv.Run(context.Background(), executor.Spec{
		Kind: executor.KindCLI, Argv: executor.CLIArgv("vendorctl"),
	}, sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(sink.joined(), "leak=unset") {
		t.Errorf("an unrelated environment variable was inherited:\n%s", sink.joined())
	}
	if !strings.Contains(sink.joined(), "path=set") {
		t.Errorf("PATH was not inherited, so no tool would resolve:\n%s", sink.joined())
	}
}

func TestValidateCLIArgs(t *testing.T) {
	t.Parallel()

	if err := executor.ValidateCLIArgs([]string{"rollout", "restart", "deploy/web", "--namespace=apps"}); err != nil {
		t.Errorf("ordinary arguments refused: %v", err)
	}
	for _, bad := range []string{"a\x00b", "a\nb", "a\tb"} {
		if err := executor.ValidateCLIArgs([]string{bad}); err == nil {
			t.Errorf("argument %q accepted, want a refusal", bad)
		}
	}
}
