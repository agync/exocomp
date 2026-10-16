package executor_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

// offlineChildVar marks the re-exec'd child of an offline-proof test: the
// parent sets it and hands the child everything it needs over the
// environment, since the child runs in its own network namespace and cannot
// be handed anything over a socket.
const offlineChildVar = "EXOCOMP_OFFLINE_TEST_CHILD"

// TestAnsiblePlaybookRunsOfflineFromVendoredCollection proves the §3.8
// offline-DR property (the offline-guarantee amendment) for Ansible: a
// collection vendored once while online is usable by a playbook run with no
// network access at all, via CollectionsPath alone.
//
// community.sops stands in for a real vendored dependency because the
// design itself depends on it. The playbook decrypts a real sops/age
// fixture through the collection's lookup plugin, so success can only mean
// the plugin resolved and ran — not that ansible silently skipped a task it
// could not satisfy.
//
// The offline half runs in a re-exec'd child under `unshare -rn`: a fresh
// user+network namespace needs no privilege on this host (verified: user
// namespaces are enabled), and re-executing this same test binary is the
// only way to put the actual subprocess the driver launches inside it,
// since the driver does not expose a namespace hook.
func TestAnsiblePlaybookRunsOfflineFromVendoredCollection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	if os.Getenv(offlineChildVar) == "1" {
		runAnsibleOfflineChild(t)
		return
	}

	requireOfflineProofTools(t, "unshare", executor.AnsibleBinary, "ansible-galaxy", "sops", "age-keygen")

	dir := t.TempDir()
	collections := filepath.Join(dir, "collections")
	if out, err := exec.Command("ansible-galaxy", "collection", "install",
		"community.sops", "-p", collections).CombinedOutput(); err != nil {
		t.Fatalf("vendor community.sops (online): %v\n%s", err, out)
	}

	ageKey := filepath.Join(dir, "age-key.txt")
	keygenOut, err := exec.Command("age-keygen", "-o", ageKey).CombinedOutput()
	if err != nil {
		t.Fatalf("age-keygen: %v\n%s", err, keygenOut)
	}
	pub := parseAgePublicKey(t, keygenOut)

	plain := filepath.Join(dir, "plain.yaml")
	const canary = "exocomp-offline-canary"
	if err := os.WriteFile(plain, []byte("secret: "+canary+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	encrypted := filepath.Join(dir, "secret.enc.yaml")
	sopsCmd := exec.Command("sops", "--age", pub, "--encrypt", plain)
	sopsCmd.Env = append(os.Environ(), "SOPS_AGE_KEY_FILE="+ageKey)
	encOut, err := sopsCmd.Output()
	if err != nil {
		t.Fatalf("sops encrypt: %v", err)
	}
	if err := os.WriteFile(encrypted, encOut, 0o600); err != nil {
		t.Fatalf("write encrypted fixture: %v", err)
	}

	playbook := filepath.Join(dir, "play.yml")
	body := "- hosts: localhost\n" +
		"  connection: local\n" +
		"  gather_facts: false\n" +
		"  tasks:\n" +
		"    - name: read a secret through the vendored collection\n" +
		"      ansible.builtin.debug:\n" +
		"        msg: \"{{ lookup('community.sops.sops', '" + encrypted + "') }}\"\n"
	if err := os.WriteFile(playbook, []byte(body), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	child := exec.Command("unshare", "-rn", "--", self,
		"-test.run", "^TestAnsiblePlaybookRunsOfflineFromVendoredCollection$", "-test.v")
	child.Env = append(os.Environ(),
		offlineChildVar+"=1",
		"EXOCOMP_OFFLINE_COLLECTIONS="+collections,
		"EXOCOMP_OFFLINE_PLAYBOOK="+playbook,
		"EXOCOMP_OFFLINE_AGE_KEY="+ageKey,
	)
	out, err := child.CombinedOutput()
	t.Logf("offline child output:\n%s", out)
	if err != nil {
		t.Fatalf("offline run failed: %v", err)
	}
	if !strings.Contains(string(out), canary) {
		t.Fatalf("offline run did not decrypt the vendored-collection secret")
	}
}

// runAnsibleOfflineChild is the body that actually runs network-denied. It
// first asserts the namespace really has no route out, so a passing test
// cannot be explained by network access it was supposed to lack.
func runAnsibleOfflineChild(t *testing.T) {
	conn, err := net.DialTimeout("tcp", "galaxy.ansible.com:443", 2*time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("galaxy.ansible.com is reachable inside the namespace; the offline proof is invalid")
	}

	sink := &collector{}
	res, err := executor.NewAnsible(executor.AnsibleOptions{
		CollectionsPath: os.Getenv("EXOCOMP_OFFLINE_COLLECTIONS"),
		Env:             []string{"SOPS_AGE_KEY_FILE=" + os.Getenv("EXOCOMP_OFFLINE_AGE_KEY")},
	}).Run(context.Background(), executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv(os.Getenv("EXOCOMP_OFFLINE_PLAYBOOK"), ""),
	}, sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ansible-playbook exited %d:\n%s", res.ExitCode, strings.Join(sink.msgs(""), "\n"))
	}
	if _, err := os.Stdout.WriteString(strings.Join(sink.msgs(""), "\n") + "\n"); err != nil {
		t.Fatalf("write child output: %v", err)
	}
}

// requireOfflineProofTools skips the test with the reason when any named
// tool is missing, rather than failing a proof that could not run at all.
func requireOfflineProofTools(t *testing.T, names ...string) {
	t.Helper()
	needUserns := false
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not on PATH: %v", name, err)
		}
		if name == "unshare" {
			needUserns = true
		}
	}
	// unshare existing is not enough: the offline proofs need an unprivileged
	// user+network namespace, which a host may forbid — GitHub's ubuntu-latest
	// runners restrict unprivileged user namespaces, and the child then fails
	// at `write /proc/self/uid_map: Operation not permitted`. Probe a bare
	// namespace and skip rather than fail the gate.
	if needUserns && exec.Command("unshare", "-rn", "true").Run() != nil {
		t.Skip("unprivileged user namespaces are unavailable")
	}
}

// parseAgePublicKey pulls the recipient out of age-keygen's stderr, which is
// the only place it prints one.
func parseAgePublicKey(t *testing.T, out []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "Public key: "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal(errors.New("age-keygen output carried no public key line"))
	return ""
}
