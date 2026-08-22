package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// TestHostOnboardRecordsKeysSeedsRunbookAndFleetKey drives onboarding against a
// stubbed keyscan so it is hermetic: it proves the recorded host keys land in
// the backed-up store, the fleet key is generated and sealed with its public
// half printed, and the seeded runbook is written into the data volume.
func TestHostOnboardRecordsKeysSeedsRunbookAndFleetKey(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()

	// Stub ssh-keyscan: return a canned two-key scan for the named host.
	orig := keyscanHost
	t.Cleanup(func() { keyscanHost = orig })
	keyscanHost = func(_ context.Context, host string) ([]byte, error) {
		return []byte("# " + host + ":22 SSH-2.0-OpenSSH\n" +
			host + " ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAstub\n" +
			host + " ssh-rsa AAAAB3NzaC1yc2Estub\n"), nil
	}

	var out, warn strings.Builder
	if err := runHostOnboard(ctx, &out, &warn, []string{"node0"}, false); err != nil {
		t.Fatalf("onboard: %v (warn: %s)", err, warn.String())
	}

	// The fleet public key is printed for the operator to authorize.
	if !strings.Contains(out.String(), "ssh-ed25519 ") || !strings.Contains(out.String(), "exocomp-fleet") {
		t.Errorf("onboard did not print the fleet public key:\n%s", out.String())
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Host keys recorded in the store (which the backup copies wholesale).
	hosts, err := st.ListKnownHosts(ctx)
	if err != nil {
		t.Fatalf("list known hosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("recorded %d host keys, want 2", len(hosts))
	}
	rendered := store.RenderKnownHosts(hosts)
	if !strings.Contains(rendered, "node0 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAstub") {
		t.Errorf("known_hosts missing the scanned ed25519 line:\n%s", rendered)
	}

	// The fleet key is sealed and re-derivable.
	if _, err := st.GetSecret(ctx, fleetKeySecret); err != nil {
		t.Fatalf("fleet key not sealed: %v", err)
	}
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()
	pub, err := ensureFleetKey(ctx, st, env)
	if err != nil {
		t.Fatalf("re-derive fleet key: %v", err)
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Errorf("re-derived fleet public key = %q", pub)
	}

	// The onboarding runbook is seeded into the data volume.
	seeded := filepath.Join(dir, "runbooks", "onboard.yml")
	body, err := os.ReadFile(seeded)
	if err != nil {
		t.Fatalf("runbook not seeded: %v", err)
	}
	if !strings.Contains(string(body), "ansible.posix.authorized_key") ||
		!strings.Contains(string(body), "sudoers.d") {
		t.Errorf("seeded runbook missing the key-push/sudoers tasks:\n%s", body)
	}
}

// TestHostOnboardAgainstLocalhost is the real ssh-to-localhost proof: if an
// sshd answers on localhost, onboarding records its live host key. It is skipped
// (not failed) where no sshd is reachable, so the suite stays hermetic.
func TestHostOnboardAgainstLocalhost(t *testing.T) {
	if _, err := exec.LookPath("ssh-keyscan"); err != nil {
		t.Skip("ssh-keyscan not on PATH")
	}
	out, err := exec.Command("ssh-keyscan", "-T", "3", "-t", "ed25519", "localhost").Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		t.Skip("no sshd reachable on localhost")
	}

	dir := initForSource(t)
	ctx := context.Background()
	var o, w strings.Builder
	if err := runHostOnboard(ctx, &o, &w, []string{"localhost"}, false); err != nil {
		t.Fatalf("onboard localhost: %v (%s)", err, w.String())
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	hosts, err := st.ListKnownHosts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hosts) == 0 {
		t.Fatal("onboarding localhost recorded no host key")
	}
	for _, h := range hosts {
		if h.Host != "localhost" {
			t.Errorf("recorded unexpected host %q", h.Host)
		}
	}
}
