package executor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeNonoRecordingGrants writes a stand-in for the nono launcher into
// dir/nono that records every --read-file and --read value it is handed into
// filesLog and exits 0, so a sandboxed run's read-grant posture can be
// asserted without a real launcher or ansible. Each line is a single
// self-describing token ("--read-file=<path>" or "--read=<path>"), so the
// file-vs-directory distinction survives the whitespace split. The log path is
// baked into the script rather than passed through the environment, because
// the ansible driver filters the sandboxed child's environment down to an
// allowlist and an env-carried path would not survive it.
func fakeNonoRecordingGrants(t *testing.T, dir, filesLog string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"log=" + filesLog + "\n" +
		"prev=\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--read-file\" ] || [ \"$prev\" = \"--read\" ]; then\n" +
		"    echo \"$prev=$arg\" >> \"$log\"\n" +
		"  fi\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"exit 0\n"
	p := filepath.Join(dir, "nono")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake nono: %v", err)
	}
	return p
}

// ansibleProfileGrants runs a sandboxed ansible job whose spec declares ssh
// identity files and returns the launcher's read-grant tokens
// ("--read-file=<path>" or "--read=<path>"). It mirrors sandboxedAnsible.Run
// through the real driver, so the append under test is the one the driver
// runs.
func ansibleProfileGrants(t *testing.T, identities []string) []string {
	t.Helper()
	data := t.TempDir()
	root := t.TempDir()
	filesLog := filepath.Join(root, "grants.log")
	fake := filepath.Join(root, "fake")
	if err := os.Mkdir(fake, 0o700); err != nil {
		t.Fatalf("mkdir fake tools: %v", err)
	}
	nono := fakeNonoRecordingGrants(t, fake, filesLog)
	ansible := fakeAnsiblePlaybook(t, fake)

	drv := NewSandboxedAnsible(AnsibleOptions{
		Binary:         ansible,
		PrivateDirRoot: t.TempDir(),
	}, SandboxOptions{DataDir: data, Binary: nono})

	res, err := drv.Run(context.Background(), Spec{
		Kind:             KindAnsible,
		Argv:             AnsibleArgv(filepath.Join(t.TempDir(), "site.yml"), ""),
		SSHIdentityFiles: identities,
	}, discardSink{})
	if err != nil {
		t.Fatalf("sandboxed ansible run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("sandboxed ansible run exited %d", res.ExitCode)
	}
	body, err := os.ReadFile(filesLog)
	if err != nil {
		t.Fatalf("read grants log: %v", err)
	}
	return strings.Fields(string(body))
}

// TestAnsibleDriverSSHIdentityDefaultDeny: a job that declares no identity
// carries no grant on the path — the widening is strictly opt-in, and the
// default-deny posture the sandbox promises is unchanged.
func TestAnsibleDriverSSHIdentityDefaultDeny(t *testing.T) {
	t.Parallel()

	identity := filepath.Join(t.TempDir(), "id_ed25519")
	if got := ansibleProfileGrants(t, nil); slices.Contains(got, "--read-file="+identity) {
		t.Fatalf("an undeclared identity %s was granted: %v", identity, got)
	}
}

// TestAnsibleDriverGrantsSpecSSHIdentity: a job declaring an identity file
// outside the source checkout gets it granted read-only as a single file.
// Dropping the ReadFiles append in sandboxedAnsible.Run fails this test — it
// is the mutation the grant lives or dies on.
func TestAnsibleDriverGrantsSpecSSHIdentity(t *testing.T) {
	t.Parallel()

	identity := filepath.Join(t.TempDir(), "id_ed25519")
	if got := ansibleProfileGrants(t, []string{identity}); !slices.Contains(got, "--read-file="+identity) {
		t.Fatalf("declared identity %s was not granted --read-file: %v", identity, got)
	}
}

// TestAnsibleDriverSSHIdentityNeverWidensToADirectory: the identity is granted
// as a single file, never as a --read directory grant — a `~/.ssh` directory
// grant would widen to every key and known_hosts beside the one declared.
func TestAnsibleDriverSSHIdentityNeverWidensToADirectory(t *testing.T) {
	t.Parallel()

	identity := filepath.Join(t.TempDir(), "id_ed25519")
	if got := ansibleProfileGrants(t, []string{identity}); slices.Contains(got, "--read="+identity) {
		t.Fatalf("identity %s widened into a --read directory grant: %v", identity, got)
	}
}

// TestAnsibleDriverRefusesIdentityOverTheDataDir: a declared identity that
// covers the daemon's own keys is refused when the profile is built, the same
// guard every grant passes — a read grant cannot be pointed at the data dir to
// read exocomp.db or host.key.
func TestAnsibleDriverRefusesIdentityOverTheDataDir(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	root := t.TempDir()
	filesLog := filepath.Join(root, "grants.log")
	fake := filepath.Join(root, "fake")
	if err := os.Mkdir(fake, 0o700); err != nil {
		t.Fatalf("mkdir fake tools: %v", err)
	}
	drv := NewSandboxedAnsible(AnsibleOptions{
		Binary:         fakeAnsiblePlaybook(t, fake),
		PrivateDirRoot: t.TempDir(),
	}, SandboxOptions{DataDir: data, Binary: fakeNonoRecordingGrants(t, fake, filesLog)})

	_, err := drv.Run(context.Background(), Spec{
		Kind:             KindAnsible,
		Argv:             AnsibleArgv(filepath.Join(t.TempDir(), "site.yml"), ""),
		SSHIdentityFiles: []string{filepath.Join(data, "host.key")},
	}, discardSink{})
	if err == nil {
		t.Fatal("an identity grant over the data dir was accepted")
	}
}
