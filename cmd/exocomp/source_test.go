package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/deps"
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

	out, err := runSource(t, "add", "nas", "--path", "/mnt/nas/infra")
	if err != nil {
		t.Fatalf("source add --path: %v (%s)", err, out)
	}
	// A path source is read-only by default.
	if !strings.Contains(out, "registered read-only path source") {
		t.Fatalf("add output = %q", out)
	}

	// --writable opts into a read+write tree.
	wout, err := runSource(t, "add", "scratch", "--path", "/mnt/scratch", "--writable")
	if err != nil {
		t.Fatalf("source add --path --writable: %v (%s)", err, wout)
	}
	if !strings.Contains(wout, "registered writable path source") {
		t.Fatalf("writable add output = %q", wout)
	}

	list, err := runSource(t, "list")
	if err != nil {
		t.Fatalf("source list: %v", err)
	}
	if !strings.Contains(list, "nas") || !strings.Contains(list, "path") ||
		!strings.Contains(list, "ro") || !strings.Contains(list, "rw") {
		t.Fatalf("list = %q", list)
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

// TestSourceAddDuplicateLeavesNoOrphanedSecret covers the name collision that
// orphans a sealed deploy key: a git add's CreateSecret (a brand-new secret
// name) can succeed while the subsequent CreateSource fails because the
// source name itself already belongs to a different (here, path-kind)
// source. The secret must not survive a source registration that never
// landed — there is no recovery UI for a sealed row nothing references.
func TestSourceAddDuplicateLeavesNoOrphanedSecret(t *testing.T) {
	dir := initForSource(t)

	if _, err := runSource(t, "add", "infra", "--path", "/mnt/infra"); err != nil {
		t.Fatalf("source add --path: %v", err)
	}

	_, err := runSource(t, "add", "infra", "--url", "git@forge:org/infra.git", "--ref", "main")
	if err == nil {
		t.Fatal("expected the duplicate source name to be refused")
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := st.GetSecret(context.Background(), "source/infra/deploy-key"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deploy key secret = %v, want ErrNotFound (no orphan from the failed add)", err)
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
	if testing.Short() {
		t.Skip("requires real git")
	}
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

// TestSourceAddSignedRequiresOwnedSigners proves the add-time half of the
// signer-trust invariant: a --signed git source with no --allowed-signers/--signer is refused at
// registration, because a signed ref must verify against exocomp's own signers,
// never the ambient host git config. Supplying an inline signer is accepted.
func TestSourceAddSignedRequiresOwnedSigners(t *testing.T) {
	initForSource(t)

	out, err := runSource(t, "add", "infra", "--url", "git@forge:org/infra.git", "--signed")
	if err == nil {
		t.Fatalf("--signed with no signers was accepted: %q", out)
	}
	if !strings.Contains(err.Error(), "allowed-signers") {
		t.Fatalf("error = %v, want it to name --allowed-signers", err)
	}

	// A signed source that names its trust is fine.
	ok, err := runSource(t, "add", "infra", "--url", "git@forge:org/infra.git", "--signed",
		"--signer", "* ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyMaterialForTest")
	if err != nil {
		t.Fatalf("signed add with a signer failed: %v (%s)", err, ok)
	}
}

// TestForgeKnownHosts covers the S5 wiring: a sync materializes exactly the
// recorded keys for the forge host into a known_hosts file, and a forge with no
// recorded key pins nothing (stays trust-on-first-use) rather than failing.
func TestForgeKnownHosts(t *testing.T) {
	dir := initForSource(t)
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	for _, kh := range []store.KnownHost{
		{Host: "forge", KeyType: "ssh-ed25519", Key: "AAAAforgekey"},
		{Host: "other", KeyType: "ssh-ed25519", Key: "AAAAotherkey"},
	} {
		if err := st.RecordKnownHost(ctx, kh); err != nil {
			t.Fatalf("record %s: %v", kh.Host, err)
		}
	}

	path, cleanup, err := forgeKnownHosts(ctx, st, "forge")
	if err != nil {
		t.Fatalf("forgeKnownHosts: %v", err)
	}
	defer cleanup()
	if path == "" {
		t.Fatal("no known_hosts file materialized for a recorded forge")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "forge ssh-ed25519 AAAAforgekey") {
		t.Errorf("known_hosts missing the forge key: %q", body)
	}
	if strings.Contains(string(body), "other") {
		t.Errorf("known_hosts leaked an unrelated host: %q", body)
	}

	// A forge with no recorded key must not pin, or a first clone could never run.
	p2, c2, err := forgeKnownHosts(ctx, st, "unrecorded")
	if err != nil {
		t.Fatalf("forgeKnownHosts(unrecorded): %v", err)
	}
	defer c2()
	if p2 != "" {
		t.Errorf("an unrecorded forge pinned a file %q, want trust-on-first-use", p2)
	}
}

// --- source rm cache GC (ADR 0015) ---

func TestSourceRmPurgesSoleSourceCaches(t *testing.T) {
	dir := initForSource(t)
	root := filepath.Join(dir, "trees", "infra")
	python, collections := plantSourceCaches(t, dir, root)

	if _, err := runSource(t, "add", "infra", "--path", root); err != nil {
		t.Fatalf("source add --path: %v", err)
	}
	if out, err := runSource(t, "rm", "infra"); err != nil {
		t.Fatalf("source rm: %v", err)
	} else if !strings.Contains(out, "removed source \"infra\" (cached project deps removed)") {
		t.Errorf("sole-source rm must say the cached project deps were removed, got: %q", out)
	}
	if _, err := os.Stat(python); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache/python kept after rm of a sole source: %v", err)
	}
	if _, err := os.Stat(collections); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache/collections kept after rm of a sole source: %v", err)
	}
}

func TestSourceRmSharedRootKeepsCaches(t *testing.T) {
	dir := initForSource(t)
	root := filepath.Join(dir, "trees", "shared")
	python, collections := plantSourceCaches(t, dir, root)

	if _, err := runSource(t, "add", "a", "--path", root); err != nil {
		t.Fatalf("source add a: %v", err)
	}
	if _, err := runSource(t, "add", "b", "--path", root); err != nil {
		t.Fatalf("source add b: %v", err)
	}

	// A sibling resolving to the same root owns the cache too; rm must not
	// purge a plane a surviving source still resolves to.
	if _, err := runSource(t, "rm", "a"); err != nil {
		t.Fatalf("source rm a: %v", err)
	}
	if _, err := os.Stat(python); err != nil {
		t.Fatalf("cache/python purged despite a sibling sharing the root: %v", err)
	}
	if _, err := os.Stat(collections); err != nil {
		t.Fatalf("cache/collections purged despite a sibling sharing the root: %v", err)
	}

	// With the last owner gone, the plane is garbage-collected.
	if out, err := runSource(t, "rm", "b"); err != nil {
		t.Fatalf("source rm b: %v", err)
	} else if !strings.Contains(out, "removed source \"b\" (cached project deps removed)") {
		t.Errorf("last-owner rm must say the cached project deps were removed, got: %q", out)
	}
	if _, err := os.Stat(python); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache/python kept after rm of the last owner: %v", err)
	}
	if _, err := os.Stat(collections); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache/collections kept after rm of the last owner: %v", err)
	}
}

func TestSourceRmDisabledSiblingKeepsCaches(t *testing.T) {
	dir := initForSource(t)
	root := filepath.Join(dir, "trees", "shared")
	python, collections := plantSourceCaches(t, dir, root)

	if _, err := runSource(t, "add", "a", "--path", root); err != nil {
		t.Fatalf("source add a: %v", err)
	}
	if _, err := runSource(t, "add", "b", "--path", root); err != nil {
		t.Fatalf("source add b: %v", err)
	}
	// A disabled source still owns its cache when re-enabled (ADR 0015), so it
	// is a reference the guard must count.
	disableSource(t, dir, "b")

	if _, err := runSource(t, "rm", "a"); err != nil {
		t.Fatalf("source rm a: %v", err)
	}
	if _, err := os.Stat(python); err != nil {
		t.Fatalf("cache/python purged despite a disabled sibling owning the root: %v", err)
	}
	if _, err := os.Stat(collections); err != nil {
		t.Fatalf("cache/collections purged despite a disabled sibling owning the root: %v", err)
	}
}

func TestSourceRmDisabledUniqueSourcePurgesCaches(t *testing.T) {
	dir := initForSource(t)
	root := filepath.Join(dir, "trees", "infra")
	python, collections := plantSourceCaches(t, dir, root)

	if _, err := runSource(t, "add", "infra", "--path", root); err != nil {
		t.Fatalf("source add: %v", err)
	}
	disableSource(t, dir, "infra")

	if _, err := runSource(t, "rm", "infra"); err != nil {
		t.Fatalf("source rm of a disabled source: %v", err)
	}
	if _, err := os.Stat(python); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache/python kept after rm of a disabled sole source: %v", err)
	}
	if _, err := os.Stat(collections); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache/collections kept after rm of a disabled sole source: %v", err)
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

// plantSourceCaches creates the dependency-cache planes a synced source leaves
// under dataDir for the given root (cache/python and cache/collections under
// the root's SourceKey, ADR 0015), returning both plane roots.
func plantSourceCaches(t *testing.T, dir, root string) (pythonDir, collectionsDir string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	key := deps.SourceKey(root)
	pythonDir = filepath.Join(dir, "cache", "python", key)
	collectionsDir = filepath.Join(dir, "cache", "collections", key)
	for _, d := range []string{
		filepath.Join(pythonDir, "venv"),
		filepath.Join(collectionsDir, "ansible_collections"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return pythonDir, collectionsDir
}

// disableSource flips a source's enabled bit off directly, the shape a web
// disable leaves behind.
func disableSource(t *testing.T, dir, name string) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	src, err := st.SourceByName(context.Background(), name)
	if err != nil {
		t.Fatalf("source by name: %v", err)
	}
	if err := st.SetSourceEnabled(context.Background(), src.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
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
