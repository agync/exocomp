package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// initForSource sets up a data dir with an envelope and returns it.
func initForSource(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	dir := filepath.Join(root, "exocomp")
	env, _, err := secrets.Init(dir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	_ = env.Close()
	return dir
}

// runCobra executes one freshly built cobra command with args, capturing output.
func runCobra(t *testing.T, factory func() *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := factory()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestSourceAddPathAndList(t *testing.T) {
	initForSource(t)

	out, err := runSource(t, "add", "nas", "--path", "/mnt/nas/infra", "--kind", "hostpath")
	if err != nil {
		t.Fatalf("source add --path: %v (%s)", err, out)
	}
	if !strings.Contains(out, "registered hostpath source") {
		t.Fatalf("add output = %q", out)
	}

	list, err := runSource(t, "list")
	if err != nil {
		t.Fatalf("source list: %v", err)
	}
	if !strings.Contains(list, "nas") || !strings.Contains(list, "hostpath") {
		t.Fatalf("list = %q", list)
	}

	// A bad path kind is refused.
	if _, err := runSource(t, "add", "bad", "--path", "/x", "--kind", "git"); err == nil {
		t.Fatal("expected --kind git to be rejected for a path source")
	}
	// Naming both --url and --path is refused.
	if _, err := runSource(t, "add", "both", "--path", "/x", "--url", "git@f:o/r.git"); err == nil {
		t.Fatal("expected --url with --path to be rejected")
	}
}

func TestSourceAddGitGeneratesAndSealsDeployKey(t *testing.T) {
	dir := initForSource(t)

	out, err := runSource(t, "add", "infra", "--url", "git@forge:org/infra.git", "--ref", "main")
	if err != nil {
		t.Fatalf("source add --url: %v (%s)", err, out)
	}
	if !strings.Contains(out, "ssh-ed25519 ") {
		t.Fatalf("add did not print the public deploy key: %q", out)
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	src, err := st.SourceByName(ctx, "infra")
	if err != nil {
		t.Fatalf("source by name: %v", err)
	}
	if src.Kind != store.SourceGit || src.DeployKeySecret != "source/infra/deploy-key" {
		t.Fatalf("source row = %+v", src)
	}

	// The sealed deploy key opens back to a usable OpenSSH private key.
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()
	row, err := st.GetSecret(ctx, src.DeployKeySecret)
	if err != nil {
		t.Fatalf("get deploy key secret: %v", err)
	}
	pem, err := env.OpenSecret(secrets.SealedSecret{
		Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open sealed deploy key: %v", err)
	}
	if !bytes.Contains(pem, []byte("OPENSSH PRIVATE KEY")) {
		t.Fatalf("sealed value is not an OpenSSH private key: %q", pem[:min(40, len(pem))])
	}
}

func TestRunSecretBindingLifecycle(t *testing.T) {
	dir := initForSource(t)

	// Binding a secret that does not exist is refused at registration.
	if _, err := runRunSecret(t, "add", "proxmox", "PROXMOX_PASSWORD", "provider/proxmox/password"); err == nil {
		t.Fatal("binding a missing secret should fail")
	}

	// Seal the secret, then bind it.
	sealSecretDirect(t, dir, "provider/proxmox/password", "s3cr3t")
	if _, err := runRunSecret(t, "add", "proxmox", "PROXMOX_PASSWORD", "provider/proxmox/password"); err != nil {
		t.Fatalf("run-secret add: %v", err)
	}

	// A malformed env name is refused.
	if _, err := runRunSecret(t, "add", "proxmox", "bad name", "provider/proxmox/password"); err == nil {
		t.Fatal("malformed env name should be rejected")
	}

	list, err := runRunSecret(t, "list", "proxmox")
	if err != nil {
		t.Fatalf("run-secret list: %v", err)
	}
	if !strings.Contains(list, "PROXMOX_PASSWORD") || !strings.Contains(list, "provider/proxmox/password") {
		t.Fatalf("list = %q", list)
	}

	if _, err := runRunSecret(t, "rm", "proxmox"); err != nil {
		t.Fatalf("run-secret rm: %v", err)
	}
	after, _ := runRunSecret(t, "list", "proxmox")
	if !strings.Contains(after, "no pairs") {
		t.Fatalf("binding not removed: %q", after)
	}
}

func TestInventoryAddWithBindings(t *testing.T) {
	initForSource(t)

	out, err := runInventory(t, "add", "proxmox",
		"--kind", "dynamic",
		"--path", "inventory/proxmox.yml",
		"--bind", "PROXMOX_PASSWORD=provider/proxmox/password",
		"--bind", "PROXMOX_USER=provider/proxmox/user")
	if err != nil {
		t.Fatalf("inventory add: %v (%s)", err, out)
	}

	list, err := runInventory(t, "list")
	if err != nil {
		t.Fatalf("inventory list: %v", err)
	}
	if !strings.Contains(list, "proxmox") || !strings.Contains(list, "binds:2") {
		t.Fatalf("list = %q", list)
	}

	// A malformed --bind is refused.
	if _, err := runInventory(t, "add", "bad", "--kind", "dynamic", "--bind", "NOEQUALS"); err == nil {
		t.Fatal("malformed --bind should be rejected")
	}
}

func TestSourceSyncFromLocalRepo(t *testing.T) {
	dir := initForSource(t)
	repo, wantSHA := localOriginRepo(t)

	// Register a git source pointing at the local repo, then sync it.
	if _, err := runSource(t, "add", "local", "--url", repo, "--ref", "v1.0.0"); err != nil {
		t.Fatalf("source add: %v", err)
	}
	out, err := runSource(t, "sync", "local")
	if err != nil {
		t.Fatalf("source sync: %v (%s)", err, out)
	}
	if !strings.Contains(out, "synced") {
		t.Fatalf("sync output = %q", out)
	}

	st, _ := store.Open(filepath.Join(dir, "exocomp.db"))
	defer func() { _ = st.Close() }()
	src, _ := st.SourceByName(context.Background(), "local")
	if src.LastSyncSHA != wantSHA {
		t.Fatalf("last_sync_sha = %q, want %q", src.LastSyncSHA, wantSHA)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "local", "site.yml")); err != nil {
		t.Fatalf("checkout missing: %v", err)
	}
}

// --- helpers ---

func runSource(t *testing.T, args ...string) (string, error) {
	return runCobra(t, newSourceCmd, args...)
}
func runRunSecret(t *testing.T, args ...string) (string, error) {
	return runCobra(t, newRunSecretCmd, args...)
}
func runInventory(t *testing.T, args ...string) (string, error) {
	return runCobra(t, newInventoryCmd, args...)
}

func sealSecretDirect(t *testing.T, dir, name, value string) {
	t.Helper()
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()
	sealed, err := env.Seal(name, "generic", []byte(value))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.CreateSecret(context.Background(), store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce,
		Ciphertext: sealed.Ciphertext, AAD: sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		t.Fatalf("create secret: %v", err)
	}
}

// cleanGitEnv is a git environment that does not inherit the parent's, so the
// pre-commit hook's GIT_DIR/GIT_INDEX_FILE cannot make git operate on the outer
// repo instead of the throwaway one.
func cleanGitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
}

func localOriginRepo(t *testing.T) (repo, headSHA string) {
	t.Helper()
	home := t.TempDir()
	repo = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = cleanGitEnv(home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "site.yml"), []byte("- hosts: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "site.yml")
	run("commit", "-m", "initial")
	run("tag", "v1.0.0")
	return repo, run("rev-parse", "HEAD")
}
