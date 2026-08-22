package source_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"exocomp.dev/exocomp/internal/source"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	data := "/data"

	cases := []struct {
		name, kind, srcName, uri string
		wantRoot                 string
		wantReadOnly             bool
		wantErr                  bool
	}{
		{name: "git under data dir", kind: source.KindGit, srcName: "infra",
			uri: "git@forge:org/infra.git", wantRoot: "/data/sources/infra"},
		{name: "hostpath is read-only", kind: source.KindHostPath, srcName: "nas",
			uri: "/mnt/nas/infra", wantRoot: "/mnt/nas/infra", wantReadOnly: true},
		{name: "volpath in the volume", kind: source.KindVolPath, srcName: "seeded",
			uri: "/data/checkouts/seeded", wantRoot: "/data/checkouts/seeded"},
		{name: "git needs a url", kind: source.KindGit, srcName: "x", uri: "", wantErr: true},
		{name: "path must be absolute", kind: source.KindVolPath, srcName: "x",
			uri: "relative/path", wantErr: true},
		{name: "unknown kind", kind: "svn", srcName: "x", uri: "y", wantErr: true},
		{name: "name cannot climb out", kind: source.KindGit, srcName: "../evil",
			uri: "git@f:o/r.git", wantErr: true},
		{name: "empty name", kind: source.KindGit, srcName: "", uri: "x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			loc, err := source.Resolve(data, tc.kind, tc.srcName, tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q) = %+v, want error", tc.srcName, loc)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if loc.Root != tc.wantRoot || loc.ReadOnly != tc.wantReadOnly {
				t.Fatalf("Resolve = %+v, want root %q readonly %v", loc, tc.wantRoot, tc.wantReadOnly)
			}
		})
	}
}

func TestGenerateDeployKey(t *testing.T) {
	t.Parallel()
	key, err := source.GenerateDeployKey("infra")
	if err != nil {
		t.Fatalf("GenerateDeployKey: %v", err)
	}
	// The private half parses as an OpenSSH key.
	if _, err := ssh.ParseRawPrivateKey(key.PrivatePEM); err != nil {
		t.Fatalf("private key does not parse: %v", err)
	}
	// The public half is a valid single authorized_keys line for the forge.
	if !strings.HasPrefix(key.PublicAuthorizedKey, "ssh-ed25519 ") {
		t.Fatalf("public key = %q, want an ssh-ed25519 line", key.PublicAuthorizedKey)
	}
	if !strings.HasSuffix(key.PublicAuthorizedKey, "exocomp-infra") {
		t.Fatalf("public key comment = %q, want exocomp-infra suffix", key.PublicAuthorizedKey)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key.PublicAuthorizedKey))
	if err != nil {
		t.Fatalf("public key not parseable as authorized key: %v", err)
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("public key type = %q", pub.Type())
	}
	// Two calls give different keys.
	key2, _ := source.GenerateDeployKey("infra")
	if string(key2.PrivatePEM) == string(key.PrivatePEM) {
		t.Fatal("GenerateDeployKey returned the same key twice")
	}
}

// --- git integration against a local bare repo (no network) ---

// gitEnv is a clean environment for the throwaway repos: it deliberately does
// not inherit the parent's, because a git hook (this repo's own pre-commit runs
// the tests) exports GIT_DIR and GIT_INDEX_FILE, which would make every `git -C
// <tmp>` operate on the outer repo instead of the temp one.
func gitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
}

func git(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv(home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// originRepo builds a local non-bare repo with one commit on main and a tag,
// usable as a file:// clone URL. It returns the repo path and the tag's commit.
func originRepo(t *testing.T) (repo, home, headSHA string) {
	t.Helper()
	home = t.TempDir()
	repo = t.TempDir()
	git(t, home, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "site.yml"), []byte("- hosts: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, home, repo, "add", "site.yml")
	git(t, home, repo, "commit", "-m", "initial")
	git(t, home, repo, "tag", "v1.0.0")
	headSHA = git(t, home, repo, "rev-parse", "HEAD")
	return repo, home, headSHA
}

func TestSyncClonesAndPins(t *testing.T) {
	repo, home, wantSHA := originRepo(t)
	ctx := context.Background()

	root := filepath.Join(t.TempDir(), "sources", "infra")
	res, err := source.Sync(ctx, source.SyncOptions{
		Root: root,
		URL:  repo,
		Ref:  "v1.0.0",
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.SHA != wantSHA {
		t.Fatalf("synced SHA = %q, want %q", res.SHA, wantSHA)
	}
	if _, err := os.Stat(filepath.Join(root, "site.yml")); err != nil {
		t.Fatalf("checkout missing site.yml: %v", err)
	}

	// A second commit + a new tag; re-sync to the new tag advances the pin, and
	// re-sync to the old tag stays put — the tree is exactly the pinned ref.
	if err := os.WriteFile(filepath.Join(repo, "site.yml"), []byte("- hosts: web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, home, repo, "commit", "-am", "second")
	git(t, home, repo, "tag", "v1.1.0")
	newSHA := git(t, home, repo, "rev-parse", "HEAD")

	res, err = source.Sync(ctx, source.SyncOptions{Root: root, URL: repo, Ref: "v1.1.0"})
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if res.SHA != newSHA {
		t.Fatalf("re-synced SHA = %q, want %q", res.SHA, newSHA)
	}

	back, err := source.Sync(ctx, source.SyncOptions{Root: root, URL: repo, Ref: "v1.0.0"})
	if err != nil {
		t.Fatalf("sync back: %v", err)
	}
	if back.SHA != wantSHA {
		t.Fatalf("pinned back to %q, want %q", back.SHA, wantSHA)
	}
	body, _ := os.ReadFile(filepath.Join(root, "site.yml"))
	if strings.TrimSpace(string(body)) != "- hosts: all" {
		t.Fatalf("tree not reset to the pinned ref: %q", body)
	}
}

func TestSyncNeedsRef(t *testing.T) {
	t.Parallel()
	_, err := source.Sync(context.Background(), source.SyncOptions{
		Root: filepath.Join(t.TempDir(), "x"), URL: "/nonexistent",
	})
	if err == nil {
		t.Fatal("sync without a ref should fail")
	}
}

func TestSyncSignedRefRefusesUnsigned(t *testing.T) {
	repo, _, _ := originRepo(t)
	// v1.0.0 is a lightweight, unsigned tag; a signed source must refuse it.
	_, err := source.Sync(context.Background(), source.SyncOptions{
		Root:   filepath.Join(t.TempDir(), "sources", "infra"),
		URL:    repo,
		Ref:    "v1.0.0",
		Signed: true,
	})
	if err == nil {
		t.Fatal("signed sync accepted an unsigned tag")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Fatalf("error = %v, want it to mention the missing signature", err)
	}
}

func TestSyncVerifiesSSHSignedTag(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available for SSH tag signing")
	}
	repo, home, _ := originRepo(t)

	// A generated ed25519 key signs the tag; its public half is the allowed
	// signer. This is the same key shape a deploy key uses.
	key, err := source.GenerateDeployKey("signer")
	if err != nil {
		t.Fatalf("generate signer: %v", err)
	}
	keyPath := filepath.Join(home, "sign")
	if err := os.WriteFile(keyPath, key.PrivatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	pubLine := key.PublicAuthorizedKey
	// allowed_signers format: <principal> <key-type> <key>
	fields := strings.Fields(pubLine)
	allowed := "signer@exocomp " + fields[0] + " " + fields[1]
	allowedPath := filepath.Join(home, "allowed_signers")
	if err := os.WriteFile(allowedPath, []byte(allowed+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sign an annotated tag with the SSH key.
	sign := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(gitEnv(home),
			"GIT_CONFIG_COUNT=3",
			"GIT_CONFIG_KEY_0=gpg.format", "GIT_CONFIG_VALUE_0=ssh",
			"GIT_CONFIG_KEY_1=user.signingkey", "GIT_CONFIG_VALUE_1="+keyPath,
			"GIT_CONFIG_KEY_2=user.email", "GIT_CONFIG_VALUE_2=signer@exocomp",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	sign("tag", "-s", "-m", "signed release", "v2.0.0")

	root := filepath.Join(t.TempDir(), "sources", "infra")
	res, err := source.Sync(context.Background(), source.SyncOptions{
		Root:               root,
		URL:                repo,
		Ref:                "v2.0.0",
		Signed:             true,
		AllowedSignersFile: allowedPath,
	})
	if err != nil {
		t.Fatalf("signed sync of a validly-signed tag failed: %v", err)
	}
	if res.SHA == "" {
		t.Fatal("no SHA from signed sync")
	}
}
