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
		writable                 bool
		wantRoot                 string
		wantReadOnly             bool
		wantErr                  bool
	}{
		{name: "git under data dir", kind: source.KindGit, srcName: "infra",
			uri: "git@forge:org/infra.git", wantRoot: "/data/sources/infra"},
		{name: "git checkout stays writable", kind: source.KindGit, srcName: "infra",
			uri: "git@forge:org/infra.git", writable: false,
			wantRoot: "/data/sources/infra", wantReadOnly: false},
		{name: "absolute path read-only by default", kind: source.KindPath, srcName: "nas",
			uri: "/mnt/nas/infra", wantRoot: "/mnt/nas/infra", wantReadOnly: true},
		{name: "absolute path writable when opted in", kind: source.KindPath, srcName: "seeded",
			uri: "/data/checkouts/seeded", writable: true,
			wantRoot: "/data/checkouts/seeded", wantReadOnly: false},
		{name: "relative path resolves under data dir", kind: source.KindPath, srcName: "rel",
			uri: "checkouts/app", wantRoot: "/data/checkouts/app", wantReadOnly: true},
		{name: "relative path writable", kind: source.KindPath, srcName: "rel",
			uri: "checkouts/app", writable: true, wantRoot: "/data/checkouts/app", wantReadOnly: false},
		{name: "relative path is cleaned", kind: source.KindPath, srcName: "rel",
			uri: "checkouts/./app/", wantRoot: "/data/checkouts/app", wantReadOnly: true},
		{name: "git needs a url", kind: source.KindGit, srcName: "x", uri: "", wantErr: true},
		{name: "relative dot names the data dir", kind: source.KindPath, srcName: "x",
			uri: ".", wantErr: true},
		{name: "relative empty is refused", kind: source.KindPath, srcName: "x",
			uri: "", wantErr: true},
		{name: "relative dotdot escapes", kind: source.KindPath, srcName: "x",
			uri: "../../etc", wantErr: true},
		{name: "relative dotdot mid-path escapes", kind: source.KindPath, srcName: "x",
			uri: "checkouts/../../etc", wantErr: true},
		{name: "unknown kind", kind: "svn", srcName: "x", uri: "y", wantErr: true},
		{name: "name cannot climb out", kind: source.KindGit, srcName: "../evil",
			uri: "git@f:o/r.git", wantErr: true},
		{name: "empty name", kind: source.KindGit, srcName: "", uri: "x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			loc, err := source.Resolve(data, tc.kind, tc.srcName, tc.uri, tc.writable)
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
	repo, home, _ := originRepo(t)
	// A valid allowed-signers file so the sync clears the ambient-trust guard and
	// reaches the signature check itself; the point here is the unsigned ref, not
	// the missing signers file (that path is TestSyncSignedRefRefusesAmbientTrust).
	key, err := source.GenerateDeployKey("signer")
	if err != nil {
		t.Fatalf("generate signer: %v", err)
	}
	fields := strings.Fields(key.PublicAuthorizedKey)
	allowedPath := filepath.Join(home, "allowed_signers")
	if err := os.WriteFile(allowedPath,
		[]byte("signer@exocomp "+fields[0]+" "+fields[1]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// v1.0.0 is a lightweight, unsigned tag; a signed source must refuse it.
	_, err = source.Sync(context.Background(), source.SyncOptions{
		Root:               filepath.Join(t.TempDir(), "sources", "infra"),
		URL:                repo,
		Ref:                "v1.0.0",
		Signed:             true,
		AllowedSignersFile: allowedPath,
	})
	if err == nil {
		t.Fatal("signed sync accepted an unsigned tag")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Fatalf("error = %v, want it to mention the missing signature", err)
	}
}

// TestSyncSignedRefRefusesAmbientTrust proves a --signed source cannot fall
// back to ambient trust: a --signed source with no exocomp-owned
// allowed-signers must refuse, never fall back to the host git config. The
// test first proves the fallback was a real hole — it stands up an
// ambient git config that trusts an attacker key and shows git itself accepts
// the attacker-signed tag under it — then shows the sync refuses that ref.
func TestSyncSignedRefRefusesAmbientTrust(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available for SSH tag signing")
	}
	repo, home, _ := originRepo(t)

	// An attacker's key signs a tag exocomp never authorized.
	key, err := source.GenerateDeployKey("attacker")
	if err != nil {
		t.Fatalf("generate attacker key: %v", err)
	}
	keyPath := filepath.Join(home, "attacker")
	if err := os.WriteFile(keyPath, key.PrivatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(key.PublicAuthorizedKey)
	allowed := "attacker@evil " + fields[0] + " " + fields[1]
	allowedPath := filepath.Join(home, "attacker_allowed_signers")
	if err := os.WriteFile(allowedPath, []byte(allowed+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "-C", repo, "tag", "-s", "-m", "attacker release", "v9.9.9")
	cmd.Env = append(gitEnv(home),
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=gpg.format", "GIT_CONFIG_VALUE_0=ssh",
		"GIT_CONFIG_KEY_1=user.signingkey", "GIT_CONFIG_VALUE_1="+keyPath,
		"GIT_CONFIG_KEY_2=user.email", "GIT_CONFIG_VALUE_2=attacker@evil",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sign attacker tag: %v\n%s", err, out)
	}

	// The ambient host git config trusts the attacker key — the exact fallback
	// the signed-source check closes. Prove git accepts the attacker tag under
	// it, so the refusal below is closing a real hole rather than an imaginary one.
	gitconfig := filepath.Join(home, ".gitconfig")
	cfg := "[gpg]\n\tformat = ssh\n[gpg \"ssh\"]\n\tallowedSignersFile = " + allowedPath + "\n"
	if err := os.WriteFile(gitconfig, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	verify := exec.Command("git", "-C", repo, "verify-tag", "v9.9.9")
	verify.Env = gitEnv(home) // GIT_CONFIG_GLOBAL points at the attacker config
	if out, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("ambient config did not accept the attacker tag, so the exploit control is not established: %v\n%s", err, out)
	}

	// The sync runs git under this same HOME; before this check existed it
	// verified against that ambient config and accepted the attacker tag. It
	// must now refuse.
	t.Setenv("HOME", home)
	_, err = source.Sync(context.Background(), source.SyncOptions{
		Root:   filepath.Join(t.TempDir(), "sources", "infra"),
		URL:    repo,
		Ref:    "v9.9.9",
		Signed: true, // AllowedSignersFile deliberately empty
	})
	if err == nil {
		t.Fatal("signed sync accepted a ref with no exocomp-owned allowed-signers")
	}
	if !strings.Contains(err.Error(), "allowed-signers") {
		t.Fatalf("error = %v, want it to name the missing allowed-signers", err)
	}
}

func TestForgeHost(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"git@forge:org/repo.git":            "forge",
		"ssh://git@forge:2222/org/repo.git": "forge",
		"ssh://forge/org/repo.git":          "forge",
		"user@host.example:path/to/repo":    "host.example",
		"https://forge/org/repo.git":        "",
		"http://forge/org/repo.git":         "",
		"git://forge/repo.git":              "",
		"file:///data/repo":                 "",
		"/data/checkouts/seeded":            "",
		"":                                  "",
	}
	for url, want := range cases {
		if got := source.ForgeHost(url); got != want {
			t.Errorf("ForgeHost(%q) = %q, want %q", url, got, want)
		}
	}
}

// fakeSSHDir writes an `ssh` stand-in that models OpenSSH's host-key check and
// then transports the git command locally. Under StrictHostKeyChecking=yes it
// reads the pinned key for the host out of UserKnownHostsFile and refuses the
// connection (exit 255) when it does not match the key the "forge" presents
// (baked into the script, since the sync runs git under a minimal env) — the
// way a real host-key mismatch fails a clone.
func fakeSSHDir(t *testing.T, presentedKey string) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
presented="` + presentedKey + `"
known=""; strict=""; host=""; cmd=""; hostset=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) case "$2" in
          UserKnownHostsFile=*) known="${2#UserKnownHostsFile=}" ;;
          StrictHostKeyChecking=*) strict="${2#StrictHostKeyChecking=}" ;;
        esac; shift 2 ;;
    -i) shift 2 ;;
    -*) shift ;;
    *) if [ -z "$hostset" ]; then host="$1"; hostset=1; else cmd="$cmd $1"; fi; shift ;;
  esac
done
if [ "$strict" = yes ] && [ -n "$known" ]; then
  pinned=$(awk -v h="$host" '$1==h {print $3; exit}' "$known")
  if [ "$pinned" != "$presented" ]; then
    echo "Host key verification failed." >&2
    exit 255
  fi
fi
exec sh -c "$cmd"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	return dir
}

// TestSyncPinsForgeHostKey proves that with a known_hosts file the sync
// verifies the forge host key strictly, so a key that does not match the pinned
// one fails the clone; a matching pin clones cleanly. Without the pin the sync
// would trust whatever key the forge presented (trust on first use).
func TestSyncPinsForgeHostKey(t *testing.T) {
	if _, err := exec.LookPath("git-upload-pack"); err != nil {
		t.Skip("git-upload-pack not available for the ssh transport")
	}
	repo, _, wantSHA := originRepo(t)

	t.Setenv("PATH", fakeSSHDir(t, "AAAArealforgekey")+string(os.PathListSeparator)+os.Getenv("PATH"))

	// ssh://forge<abs repo path>: git connects to host "forge" and runs
	// git-upload-pack on the repo path, which the fake ssh serves locally.
	url := "ssh://forge" + repo

	writeKH := func(body string) string {
		p := filepath.Join(t.TempDir(), "known_hosts")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A matching pin clones.
	res, err := source.Sync(context.Background(), source.SyncOptions{
		Root:           filepath.Join(t.TempDir(), "ok"),
		URL:            url,
		Ref:            "v1.0.0",
		KnownHostsFile: writeKH("forge ssh-ed25519 AAAArealforgekey\n"),
	})
	if err != nil {
		t.Fatalf("sync with a matching forge host key failed: %v", err)
	}
	if res.SHA != wantSHA {
		t.Fatalf("synced SHA = %q, want %q", res.SHA, wantSHA)
	}

	// A mismatching pin fails the sync — the host key changed under us.
	_, err = source.Sync(context.Background(), source.SyncOptions{
		Root:           filepath.Join(t.TempDir(), "bad"),
		URL:            url,
		Ref:            "v1.0.0",
		KnownHostsFile: writeKH("forge ssh-ed25519 AAAAattackerkey\n"),
	})
	if err == nil {
		t.Fatal("sync accepted a forge host key that did not match the pin")
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
