package executor

import (
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// cliJobDir builds a real job tree so the grant assertions below name the same
// paths a run would.
func cliJobDir(t *testing.T) jobDir {
	t.Helper()
	job, cleanup, err := newJobDir(t.TempDir())
	if err != nil {
		t.Fatalf("job dir: %v", err)
	}
	t.Cleanup(func() { cleanup(SinkFunc(func(Event) {})) })
	return job
}

// TestCLIProfileIsDefaultDenyEgress pins the kind's network posture: the CLI
// kind grants no port at all, unlike ansible (22) and tofu (443), because the
// kind is every allow-listed binary at once.
func TestCLIProfileIsDefaultDenyEgress(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	argv, err := ProfileFor(KindCLI, SandboxOptions{DataDir: data}).Command([]string{"/usr/bin/kubectl", "get", "pods"})
	if err != nil {
		t.Fatalf("build cli command: %v", err)
	}
	if !slices.Contains(argv, "--block-net") {
		t.Errorf("cli profile does not block outbound network: %v", argv)
	}
	if slices.Contains(argv, "--allow-connect-port") {
		t.Errorf("cli profile grants egress to the kind at large: %v", argv)
	}
	for _, a := range argv {
		if len(a) >= len("--allow-unix-socket") && a[:len("--allow-unix-socket")] == "--allow-unix-socket" {
			t.Errorf("cli profile grants a unix socket: %v", argv)
		}
	}
	sep := slices.Index(argv, "--")
	if sep < 0 || !slices.Equal(argv[sep+1:], []string{"/usr/bin/kubectl", "get", "pods"}) {
		t.Errorf("wrapped argv = %v", argv)
	}
}

// TestCLISandboxGrantsAreTheJobTreeAndTheTool: what a governed CLI run may
// touch is its own private/work/temp directories and the tool it was
// registered with. Anything wider — the data dir, the operator's home, another
// job's tree — is the failure this asserts against.
func TestCLISandboxGrantsAreTheJobTreeAndTheTool(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	job := cliJobDir(t)
	// A real script on PATH would drag its interpreter tree in; a path that does
	// not resolve contributes no grant, which keeps this assertion exact.
	sb := cliSandboxOptions(SandboxOptions{DataDir: data}, CLIOptions{
		Tool:   "vendorctl",
		Binary: filepath.Join(t.TempDir(), "absent", "vendorctl"),
	}, job)

	profile := ProfileFor(KindCLI, sb)
	for _, want := range job.grants() {
		if !slices.Contains(profile.Write, want) {
			t.Errorf("job directory %s is not granted: %v", want, profile.Write)
		}
	}
	if slices.Contains(profile.Write, job.home) {
		t.Error("the redirected home is granted; it must be created and not granted")
	}
	if len(profile.ConnectPorts) != 0 {
		t.Errorf("a tool that declared no egress got ports %v", profile.ConnectPorts)
	}
	if err := profile.Validate(); err != nil {
		t.Errorf("the assembled profile does not pass the data-dir guard: %v", err)
	}
}

// TestCLISandboxEgressIsPerRegistration: only a tool whose allow-list entry
// declared https gets a port, and it gets exactly 443.
func TestCLISandboxEgressIsPerRegistration(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	job := cliJobDir(t)
	sb := cliSandboxOptions(SandboxOptions{DataDir: data}, CLIOptions{
		Tool: "vendorctl", Binary: filepath.Join(t.TempDir(), "absent", "vendorctl"), AllowHTTPS: true,
	}, job)

	argv, err := ProfileFor(KindCLI, sb).Command([]string{"/x/vendorctl"})
	if err != nil {
		t.Fatalf("build cli command: %v", err)
	}
	var ports []string
	for i := range argv {
		if argv[i] == "--allow-connect-port" && i+1 < len(argv) {
			ports = append(ports, argv[i+1])
		}
	}
	if !slices.Equal(ports, []string{strconv.Itoa(HTTPSPort)}) {
		t.Errorf("egress ports = %v, want [%d]", ports, HTTPSPort)
	}
}

// TestSandboxedCLIIsTheCLIDriverUnderNono guards the composition: the sandboxed
// constructor must wrap the same driver, not a second implementation whose
// allow-list check could drift from the tested one.
func TestSandboxedCLIIsTheCLIDriverUnderNono(t *testing.T) {
	t.Parallel()

	drv, ok := NewSandboxedCLI(CLIOptions{Tool: "vendorctl"}, SandboxOptions{DataDir: t.TempDir()}).(*sandboxedCLI)
	if !ok {
		t.Fatal("NewSandboxedCLI did not return the sandboxed CLI driver")
	}
	if drv.opts.Tool != "vendorctl" {
		t.Errorf("the allow-listed tool did not reach the driver: %+v", drv.opts)
	}
}
