package executor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeNonoLauncher writes a stand-in for the nono launcher into dir/nono that
// records every --allow-connect-port value it is handed into portsLog and
// exits 0, so a sandboxed run's egress posture can be asserted without a real
// launcher or ansible. The log path is baked into the script rather than
// passed through the environment, because the ansible driver filters the
// sandboxed child's environment down to an allowlist and an env-carried path
// would not survive it.
func fakeNonoLauncher(t *testing.T, dir, portsLog string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"log=" + portsLog + "\n" +
		"prev=\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--allow-connect-port\" ]; then\n" +
		"    echo \"$arg\" >> \"$log\"\n" +
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

// fakeAnsiblePlaybook writes a no-op ansible-playbook into dir. It never
// runs — the fake nono launcher swallows the whole invocation — but the sandbox
// read-grants the tool the driver resolves, so it must exist and be executable.
func fakeAnsiblePlaybook(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "ansible-playbook")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake ansible-playbook: %v", err)
	}
	return p
}

// ansibleProfilePorts runs a sandboxed ansible job whose spec declares egress
// and returns the --allow-connect-port values the launcher was handed, in the
// profile's sorted order. It mirrors sandboxedAnsible.Run the way
// tofuProfilePorts mirrors sandboxedTofu.Run, but through the real driver, so
// the append under test is the one the driver runs.
func ansibleProfilePorts(t *testing.T, egress []int) []string {
	t.Helper()
	data := t.TempDir()
	root := t.TempDir()
	portsLog := filepath.Join(root, "ports.log")
	fake := filepath.Join(root, "fake")
	if err := os.Mkdir(fake, 0o700); err != nil {
		t.Fatalf("mkdir fake tools: %v", err)
	}
	nono := fakeNonoLauncher(t, fake, portsLog)
	ansible := fakeAnsiblePlaybook(t, fake)

	drv := NewSandboxedAnsible(AnsibleOptions{
		Binary:         ansible,
		PrivateDirRoot: t.TempDir(),
	}, SandboxOptions{DataDir: data, Binary: nono})

	res, err := drv.Run(context.Background(), Spec{
		Kind:        KindAnsible,
		Argv:        AnsibleArgv(filepath.Join(t.TempDir(), "site.yml"), ""),
		EgressPorts: egress,
	}, discardSink{})
	if err != nil {
		t.Fatalf("sandboxed ansible run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("sandboxed ansible run exited %d", res.ExitCode)
	}
	body, err := os.ReadFile(portsLog)
	if err != nil {
		t.Fatalf("read ports log: %v", err)
	}
	return strings.Fields(string(body))
}

// TestAnsibleDriverEgressDefaultDeny: a job that declares no egress keeps the
// kind's built-in SSH port and nothing else — the widening is strictly opt-in,
// and the default-deny posture the sandbox promises is unchanged.
func TestAnsibleDriverEgressDefaultDeny(t *testing.T) {
	t.Parallel()

	if got := ansibleProfilePorts(t, nil); !slices.Equal(got, []string{"22"}) {
		t.Fatalf("default ansible egress = %v, want [22]", got)
	}
}

// TestAnsibleDriverGrantsSpecEgress: a job declaring 2222 gets it on top of the
// kind's port 22, sorted and de-duplicated by ProfileFor. Dropping the
// ConnectPorts append in sandboxedAnsible.Run fails this test — it is the
// mutation the grant lives or dies on.
func TestAnsibleDriverGrantsSpecEgress(t *testing.T) {
	t.Parallel()

	if got := ansibleProfilePorts(t, []int{2222, 22}); !slices.Equal(got, []string{"22", "2222"}) {
		t.Fatalf("declared ansible egress = %v, want [22 2222]", got)
	}
}
