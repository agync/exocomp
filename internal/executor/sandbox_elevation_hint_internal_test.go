package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// becameDeniedLine is the loader line a sandboxed become's sudo prints when
// the un-granted sudo is refused — the signature the hint fires on.
const becameDeniedLine = "sudo: error while loading shared libraries: " +
	"libsudo_util.so.0: cannot open shared object file"

// forwardingNono writes a stand-in for the nono launcher that forwards to the
// wrapped command (the argv after `--`) instead of enforcing. The elevation
// hint is about the driver's failure enrichment, not nono's own behaviour, so
// the fake lets the stand-in ansible-playbook run and print the stderr the
// failure carries.
func forwardingNono(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "nono")
	script := "#!/bin/sh\n" +
		"i=1\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = \"--\" ]; then\n" +
		"    break\n" +
		"  fi\n" +
		"  i=$((i+1))\n" +
		"done\n" +
		"shift $i\n" +
		"exec \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake nono: %v", err)
	}
	return p
}

// fakeAnsiablePlaybook writes a stand-in ansible-playbook whose body is script.
func fakeAnsiablePlaybook(t *testing.T, dir, script string) string {
	t.Helper()
	p := filepath.Join(dir, "ansible-playbook")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake ansible-playbook: %v", err)
	}
	return p
}

// runSandboxedEligible runs a sandboxed ansible job whose fake playbook
// executes script, collecting the emitted events. privileged sends the run
// down the unsandboxed path the driver honours for a --privileged job.
func runSandboxedEligible(t *testing.T, script string, privileged bool) (Result, *collectSink) {
	t.Helper()
	return runSandboxedSpec(t, script, Spec{
		Kind:       KindAnsible,
		Argv:       AnsibleArgv(filepath.Join(t.TempDir(), "site.yml"), ""),
		Privileged: privileged,
	})
}

// runSandboxedSpec is runSandboxedEligible's general form: it runs the fake
// playbook through the real sandboxed driver with the spec under test, so a
// test can vary the argv (a local inventory, an ad-hoc pattern) and the
// declared identity files without rebuilding the harness.
func runSandboxedSpec(t *testing.T, script string, spec Spec) (Result, *collectSink) {
	t.Helper()
	data := t.TempDir()
	fake := filepath.Join(t.TempDir(), "fake")
	if err := os.Mkdir(fake, 0o700); err != nil {
		t.Fatalf("mkdir fake tools: %v", err)
	}
	nono := forwardingNono(t, fake)
	playbook := fakeAnsiablePlaybook(t, fake, script)

	drv := NewSandboxedAnsible(AnsibleOptions{
		Binary:         playbook,
		PrivateDirRoot: t.TempDir(),
	}, SandboxOptions{DataDir: data, Binary: nono})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var sink collectSink
	res, err := drv.Run(ctx, spec, &sink)
	if err != nil {
		t.Fatalf("sandboxed ansible run: %v", err)
	}
	return res, &sink
}

// collectSink keeps every event a driver emitted, so a test can assert on the
// run's output lines and on the hint line the enrichment appends.
type collectSink struct {
	mu     sync.Mutex
	events []Event
}

func (c *collectSink) Emit(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *collectSink) msgs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		out = append(out, e.Msg)
	}
	return out
}

func (c *collectSink) has(msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.Msg == msg {
			return true
		}
	}
	return false
}

// TestSandboxedAnsibleElevationHintFiresOnStdoutSignature: ansible reports the
// become failure on STDOUT, which the shell driver maps to LevelInfo, so the
// observer must see the signature wherever it lands — this is production job
// #5's shape and the regression this pins. A stderr-only observer is red here.
func TestSandboxedAnsibleElevationHintFiresOnStdoutSignature(t *testing.T) {
	t.Parallel()

	script := "echo \"" + becameDeniedLine + "\"\nexit 1\n"
	res, sink := runSandboxedEligible(t, script, false)
	if res.ExitCode != 1 {
		t.Fatalf("run exit = %d, want the fake playbook's 1", res.ExitCode)
	}
	if !sink.has(elevationHintCopy) {
		t.Errorf("a failed sandboxed run whose stdout carried the become-denied signature gained no hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
}

// TestSandboxedAnsibleElevationHintFiresOnStderrSignature: the signature on
// stderr fires too (belt and braces); the loader message's channel varies with
// the tool, so the match must not depend on which stream carried it.
func TestSandboxedAnsibleElevationHintFiresOnStderrSignature(t *testing.T) {
	t.Parallel()

	script := "echo \"" + becameDeniedLine + "\" >&2\nexit 1\n"
	res, sink := runSandboxedEligible(t, script, false)
	if res.ExitCode != 1 {
		t.Fatalf("run exit = %d, want the fake playbook's 1", res.ExitCode)
	}
	if !sink.has(elevationHintCopy) {
		t.Errorf("a failed sandboxed run with the become-denied signature gained no hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
}

// TestSandboxedAnsibleElevationHintIgnoresAPlainTaskFailure: a sandboxed run
// that fails without the signature (a plain task error) gains nothing — the
// hint is for the sandbox denial, not for any failure. Making becomeDenied
// always true makes this red.
func TestSandboxedAnsibleElevationHintIgnoresAPlainTaskFailure(t *testing.T) {
	t.Parallel()

	script := "echo \"fatal: [localhost]: FAILED! => {\\\"msg\\\": \\\"the task blew up\\\"}\" >&2\nexit 1\n"
	res, sink := runSandboxedEligible(t, script, false)
	if res.ExitCode != 1 {
		t.Fatalf("run exit = %d, want the fake playbook's 1", res.ExitCode)
	}
	if sink.has(elevationHintCopy) {
		t.Errorf("a plain task failure gained the elevation hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
}

// TestSandboxedAnsibleElevationHintNeverOnAPrivilegedRun: the hint explains a
// sandbox denial, and a privileged run is unsandboxed by definition — the same
// signature-carrying failure must gain nothing there. Enriching before the
// runUnsandboxed early return makes this red.
func TestSandboxedAnsibleElevationHintNeverOnAPrivilegedRun(t *testing.T) {
	t.Parallel()

	script := "echo \"" + becameDeniedLine + "\" >&2\nexit 1\n"
	res, sink := runSandboxedEligible(t, script, true)
	if res.ExitCode != 1 {
		t.Fatalf("run exit = %d, want the fake playbook's 1", res.ExitCode)
	}
	if sink.has(elevationHintCopy) {
		t.Errorf("a privileged run gained the elevation hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
}

// TestBecomeDeniedMatchesOnlyTheSignature: the matcher fires on the loader
// line and nothing else — a line naming sudo without the loader error, a
// loader error that does not name sudo, or a run that never prints either.
func TestBecomeDeniedMatchesOnlyTheSignature(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		msg  string
		want bool
	}{
		{"the loader-denied sudo line", becameDeniedLine, true},
		{"the line buried in other output", "TASK [become] " + becameDeniedLine + " fatal:", true},
		{"a sudo line without the loader error", "sudo: a password is required", false},
		{"a loader error that does not name sudo",
			"uname: error while loading shared libraries: libuname.so.0: cannot open shared object file", false},
		{"a plain task failure", "fatal: [localhost]: FAILED! => {\"msg\": \"boom\"}", false},
		{"an empty line", "", false},
	} {
		if got := becomeDenied(tc.msg); got != tc.want {
			t.Errorf("%s: becomeDenied = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// keyDeniedLine is the line ssh prints when the sandbox refuses the read of an
// identity key it was told to load — the key-read signature the hint fires on.
const keyDeniedLine = `Load key "/home/op/.ssh/id_ed25519": Permission denied`

// TestSandboxedAnsibleKeyReadHintFiresOnStdoutSignature: a failed sandboxed run
// whose output carries the key-read signature gains the --ssh-identity hint.
// Dropping the sshKeyDenied check in emitSandboxHints makes this red.
func TestSandboxedAnsibleKeyReadHintFiresOnStdoutSignature(t *testing.T) {
	t.Parallel()

	script := "echo '" + keyDeniedLine + "'\nexit 1\n"
	res, sink := runSandboxedEligible(t, script, false)
	if res.ExitCode != 1 {
		t.Fatalf("run exit = %d, want the fake playbook's 1", res.ExitCode)
	}
	if !sink.has(sshIdentityHintCopy) {
		t.Errorf("a failed sandboxed run whose output carried the key-read signature gained no hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
}

// TestSandboxedAnsibleKeyReadHintFiresOnStderrSignature: the signature on
// stderr fires too; the channel varies with the tool, so the match must not
// depend on which stream carried it.
func TestSandboxedAnsibleKeyReadHintFiresOnStderrSignature(t *testing.T) {
	t.Parallel()

	script := "echo '" + keyDeniedLine + "' >&2\nexit 1\n"
	res, sink := runSandboxedEligible(t, script, false)
	if res.ExitCode != 1 {
		t.Fatalf("run exit = %d, want the fake playbook's 1", res.ExitCode)
	}
	if !sink.has(sshIdentityHintCopy) {
		t.Errorf("a failed sandboxed run with the key-read signature gained no hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
}

// TestSandboxedAnsibleKeyReadHintSuppressedWhenIdentityDeclared: a run that
// already declared --ssh-identity holds the grant, so the key it named is not
// the one that failed and the hint is not repeated. Removing the
// len(spec.SSHIdentityFiles) == 0 guard makes this red.
func TestSandboxedAnsibleKeyReadHintSuppressedWhenIdentityDeclared(t *testing.T) {
	t.Parallel()

	script := "echo '" + keyDeniedLine + "'\nexit 1\n"
	res, sink := runSandboxedSpec(t, script, Spec{
		Kind:             KindAnsible,
		Argv:             AnsibleArgv(filepath.Join(t.TempDir(), "site.yml"), ""),
		SSHIdentityFiles: []string{filepath.Join(t.TempDir(), "id_ed25519")},
	})
	if res.ExitCode != 1 {
		t.Fatalf("run exit = %d, want the fake playbook's 1", res.ExitCode)
	}
	if sink.has(sshIdentityHintCopy) {
		t.Errorf("a run that already declared --ssh-identity repeated the key hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
}

// TestSandboxedAnsibleKeyReadHintNeverOnSuccess: the signature only enriches a
// FAILED run; a successful run that happens to print the line gains nothing.
// Dropping the res.ExitCode != 0 guard makes this red.
func TestSandboxedAnsibleKeyReadHintNeverOnSuccess(t *testing.T) {
	t.Parallel()

	script := "echo '" + keyDeniedLine + "'\nexit 0\n"
	res, sink := runSandboxedEligible(t, script, false)
	if res.ExitCode != 0 {
		t.Fatalf("run exit = %d, want the fake playbook's 0", res.ExitCode)
	}
	if sink.has(sshIdentityHintCopy) {
		t.Errorf("a successful run gained the key hint:\n%s", strings.Join(sink.msgs(), "\n"))
	}
}

// TestSandboxedAnsibleElevationHintNamesLocalConnection: when the run's
// inventory is the controller itself (`localhost,`), the elevation hint blames
// the connection type and points at ssh-to-self, not the task. Dropping the
// localConnectionTarget branch in emitSandboxHints makes this red.
func TestSandboxedAnsibleElevationHintNamesLocalConnection(t *testing.T) {
	t.Parallel()

	script := "echo \"" + becameDeniedLine + "\" >&2\nexit 1\n"
	res, sink := runSandboxedSpec(t, script, Spec{
		Kind: KindAnsible,
		Argv: AnsibleArgv(filepath.Join(t.TempDir(), "site.yml"), "localhost,"),
	})
	if res.ExitCode != 1 {
		t.Fatalf("run exit = %d, want the fake playbook's 1", res.ExitCode)
	}
	if !sink.has(elevationLocalHintCopy) {
		t.Errorf("a local-connection elevation denial gained no local hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
	if sink.has(elevationHintCopy) {
		t.Errorf("a local-connection elevation denial also got the generic hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
}

// TestSandboxedAnsibleElevationHintGenericWhenNotLocal: a run whose inventory
// is not the controller gets the generic elevation hint, not the local one.
func TestSandboxedAnsibleElevationHintGenericWhenNotLocal(t *testing.T) {
	t.Parallel()

	script := "echo \"" + becameDeniedLine + "\" >&2\nexit 1\n"
	res, sink := runSandboxedSpec(t, script, Spec{
		Kind: KindAnsible,
		Argv: AnsibleArgv(filepath.Join(t.TempDir(), "site.yml"), "/etc/ansible/hosts"),
	})
	if res.ExitCode != 1 {
		t.Fatalf("run exit = %d, want the fake playbook's 1", res.ExitCode)
	}
	if !sink.has(elevationHintCopy) {
		t.Errorf("a non-local elevation denial gained no generic hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
	if sink.has(elevationLocalHintCopy) {
		t.Errorf("a non-local elevation denial got the local hint:\n%s",
			strings.Join(sink.msgs(), "\n"))
	}
}

// TestSshKeyDeniedMatchesOnlyTheSignature: the matcher fires on ssh's key-load,
// publickey and missing-identity lines and nothing else.
func TestSshKeyDeniedMatchesOnlyTheSignature(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		msg  string
		want bool
	}{
		{"the key-load denial", keyDeniedLine, true},
		{"the generic publickey failure", "fatal: [host]: UNREACHABLE! ... Permission denied (publickey).", true},
		{"the missing-identity line", "no such identity: /keys/fleet: No such file or directory", true},
		{"a loader-denied sudo line", becameDeniedLine, false},
		{"a plain task failure", "fatal: [localhost]: FAILED! => {\"msg\": \"boom\"}", false},
		{"an empty line", "", false},
	} {
		if got := sshKeyDenied(tc.msg); got != tc.want {
			t.Errorf("%s: sshKeyDenied = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLocalConnectionTarget pins which argvs reach the controller itself: an
// explicit `localhost,` inventory, or an ad-hoc loopback pattern with no
// inventory. A remote inventory, a non-loopback pattern, or a playbook argv
// with a real inventory is not a local connection.
func TestLocalConnectionTarget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{"playbook with localhost, inline", AnsibleArgv("/src/site.yml", "localhost,"), true},
		{"playbook with a real inventory", AnsibleArgv("/src/site.yml", "/etc/ansible/hosts"), false},
		{"playbook with no inventory", AnsibleArgv("/src/site.yml", ""), false},
		{"ad-hoc localhost, no inventory", AnsibleAdhocArgv("localhost", "", "command", "hostname"), true},
		{"ad-hoc 127.0.0.1, no inventory", AnsibleAdhocArgv("127.0.0.1", "", "command", "hostname"), true},
		{"ad-hoc remote pattern", AnsibleAdhocArgv("web", "", "command", "hostname"), false},
		{"ad-hoc localhost with a real inventory",
			AnsibleAdhocArgv("localhost", "/etc/ansible/hosts", "command", "hostname"), false},
	} {
		if got := localConnectionTarget(tc.argv); got != tc.want {
			t.Errorf("%s: localConnectionTarget = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSandboxedAnsiblePrivilegedPinsScratchOffTheSource: an unsandboxed run
// still gets ansible's local temp and ssh control directory on per-job paths and its
// remote temp pinned,
// so a read-only source checkout whose ansible.cfg sets them relative cannot
// fail the run at startup. Dropping runPrivileged's env makes this red.
func TestSandboxedAnsiblePrivilegedPinsScratchOffTheSource(t *testing.T) {
	t.Parallel()

	script := "echo \"local_tmp=$ANSIBLE_LOCAL_TEMP\"\necho \"control=$ANSIBLE_SSH_CONTROL_PATH_DIR\"\n" +
		"echo \"remote_tmp=$ANSIBLE_REMOTE_TMP\"\n" +
		"[ -d \"$ANSIBLE_LOCAL_TEMP\" ] && [ -d \"$ANSIBLE_SSH_CONTROL_PATH_DIR\" ] && echo dirs-exist\n"
	res, sink := runSandboxedEligible(t, script, true)
	if res.ExitCode != 0 {
		t.Fatalf("run exit = %d, want 0:\n%s", res.ExitCode, strings.Join(sink.msgs(), "\n"))
	}
	out := strings.Join(sink.msgs(), "\n")
	for _, want := range []string{"local_tmp=/", "control=/", "remote_tmp=" + remoteTmp, "dirs-exist"} {
		if !strings.Contains(out, want) {
			t.Errorf("privileged run output lacks %q:\n%s", want, out)
		}
	}
}
