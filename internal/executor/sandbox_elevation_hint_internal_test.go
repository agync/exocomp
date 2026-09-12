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
	res, err := drv.Run(ctx, Spec{
		Kind:       KindAnsible,
		Argv:       AnsibleArgv(filepath.Join(t.TempDir(), "site.yml"), ""),
		Privileged: privileged,
	}, &sink)
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

// TestSandboxedAnsibleElevationHintFiresOnTheSignature: a sandboxed run whose
// stderr carries the become-denied signature — the loader refusing the
// un-granted sudo — gains the actionable line, so the operator reads
// enforcement, not a broken host. Flipping the signature match in becomeDenied
// makes this red.
func TestSandboxedAnsibleElevationHintFiresOnTheSignature(t *testing.T) {
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
		name  string
		lines []string
		want  bool
	}{
		{"the loader-denied sudo line", []string{becameDeniedLine}, true},
		{"the line buried in other output", []string{"TASK [become]", becameDeniedLine, "fatal:"}, true},
		{"a sudo line without the loader error", []string{"sudo: a password is required"}, false},
		{"a loader error that does not name sudo",
			[]string{"uname: error while loading shared libraries: libuname.so.0: cannot open shared object file"}, false},
		{"a plain task failure", []string{"fatal: [localhost]: FAILED! => {\"msg\": \"boom\"}"}, false},
		{"empty stderr", nil, false},
	} {
		if got := becomeDenied(tc.lines); got != tc.want {
			t.Errorf("%s: becomeDenied = %v, want %v", tc.name, got, tc.want)
		}
	}
}
