package packaging

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/backup"
)

// gitTools are what the git hand restore and these tests need on PATH.
var gitTools = []string{"sh", "git", "age", "age-keygen", "zstd", "gzip", "tar", "openssl", "sha256sum", "awk"}

const gitTargetName = "nas"

// tarEntry is one member of a snapshot archive.
type tarEntry struct {
	name     string
	typeflag byte
	body     []byte
	link     string
}

// rewrite describes the snapshot a test puts on the backup branch in place of
// the one the git leg wrote. Everything the leg authenticates is recomputed
// with the target's real subkey, so a test breaks exactly one thing.
type rewrite struct {
	parts  int
	gzip   bool
	mutate func(entries []tarEntry, key []byte) []tarEntry
	// breakParts changes a part's bytes after the parts manifest was written.
	breakParts func(parts map[string][]byte)
	// noPartsTag leaves the parts manifest without its tag, and badPartsKey
	// tags it under a key that is not the target's.
	noPartsTag, badPartsKey bool
	// forgeKeyring replaces keyring.age with one wrapping a DEK of the
	// attacker's choosing, to the same public recipient, and tags everything
	// under that DEK's subkey.
	forgeKeyring bool
	// target is the name the tags are keyed to, when not the fixture's.
	target string
	// recovery replaces RECOVERY.md, and beside runs after the files are
	// written and before the commit, to add whatever else a remote can hold.
	recovery []byte
	beside   func(t *testing.T, clone string)
	// bodySnapshot changes the snapshot name the parts manifest records.
	bodySnapshot string
}

func run(t *testing.T, stdin []byte, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return out
}

// rewriteSnapshot replaces the backup branch's snapshot with one built from the
// real one, as rw says.
func rewriteSnapshot(t *testing.T, bare, identity string, rw rewrite) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", "--quiet", "--branch", "backup", bare, clone)

	manifests, _ := filepath.Glob(filepath.Join(clone, "snapshot-*.age.parts.json"))
	if len(manifests) != 1 {
		t.Fatalf("want one parts manifest in the clone, got %v", manifests)
	}
	body, err := os.ReadFile(manifests[0])
	if err != nil {
		t.Fatalf("read parts manifest: %v", err)
	}
	var old backup.PartsManifest
	if err := json.Unmarshal(body, &old); err != nil {
		t.Fatalf("parse parts manifest: %v", err)
	}

	// Open the real snapshot.
	var sealed []byte
	for _, p := range old.Parts {
		part, err := os.ReadFile(filepath.Join(clone, p.Name))
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		sealed = append(sealed, part...)
	}
	compressed := run(t, sealed, "age", "-d", "-i", identity)
	raw := run(t, compressed, "zstd", "-d", "-q", "-c")
	var entries []tarEntry
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read snapshot archive: %v", err)
		}
		data, _ := io.ReadAll(tr)
		entries = append(entries, tarEntry{name: hdr.Name, typeflag: tar.TypeReg, body: data})
	}

	recipient := strings.TrimSpace(string(run(t, nil, "age-keygen", "-y", identity)))
	dek := run(t, nil, "age", "-d", "-i", identity, filepath.Join(clone, "keyring.age"))
	if rw.forgeKeyring {
		dek = bytes.Repeat([]byte{9}, len(dek))
		forged := run(t, dek, "age", "-r", recipient)
		if err := os.WriteFile(filepath.Join(clone, "keyring.age"), forged, 0o644); err != nil {
			t.Fatalf("write forged keyring: %v", err)
		}
	}
	name := gitTargetName
	if rw.target != "" {
		name = rw.target
	}
	key, err := backup.ManifestMACKey(dek, name)
	if err != nil {
		t.Fatalf("derive manifest key: %v", err)
	}
	if rw.mutate != nil {
		entries = rw.mutate(entries, key)
	}
	if rw.forgeKeyring {
		entries[entryNamed(entries, "MANIFEST.mac")].body =
			macFileBytes(t, name, key, entries[entryNamed(entries, "MANIFEST.json")].body, "MANIFEST.json")
	}

	// Build the replacement.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0o600, Size: int64(len(e.body)), Linkname: e.link, Format: tar.FormatPAX}
		if e.typeflag != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(e.body[:hdr.Size]); err != nil {
			t.Fatalf("write tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	snapshot := old.Snapshot
	var packed []byte
	if rw.gzip {
		var zb bytes.Buffer
		zw := gzip.NewWriter(&zb)
		_, _ = zw.Write(buf.Bytes())
		_ = zw.Close()
		packed = zb.Bytes()
		snapshot = strings.Replace(snapshot, ".tar.zst.", ".tar.gz.", 1)
	} else {
		packed = run(t, buf.Bytes(), "zstd", "-q", "-c")
	}
	newSealed := run(t, packed, "age", "-r", recipient)

	// Clear the old snapshot's files.
	for _, p := range old.Parts {
		_ = os.Remove(filepath.Join(clone, p.Name))
	}
	_ = os.Remove(manifests[0])
	_ = os.Remove(manifests[0] + ".mac")

	n := max(rw.parts, 1)
	size := len(newSealed)/n + 1
	whole := sha256.Sum256(newSealed)
	recorded := snapshot
	if rw.bodySnapshot != "" {
		recorded = rw.bodySnapshot
	}
	pm := backup.PartsManifest{Snapshot: recorded, Size: int64(len(newSealed)), SHA256: hex.EncodeToString(whole[:])}
	parts := map[string][]byte{}
	digits := max(2, len(strconv.Itoa(n-1)))
	for i := range n {
		piece := newSealed[min(i*size, len(newSealed)):min((i+1)*size, len(newSealed))]
		name := fmt.Sprintf("%s.part%0*d", snapshot, digits, i)
		sum := sha256.Sum256(piece)
		pm.Parts = append(pm.Parts, backup.PartEntry{Name: name, Size: int64(len(piece)), SHA256: hex.EncodeToString(sum[:])})
		parts[name] = piece
	}
	if rw.breakParts != nil {
		rw.breakParts(parts)
	}
	for name, piece := range parts {
		if err := os.WriteFile(filepath.Join(clone, name), piece, 0o644); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	pmBody, err := json.MarshalIndent(pm, "", "  ")
	if err != nil {
		t.Fatalf("marshal parts manifest: %v", err)
	}
	pmBody = append(pmBody, '\n')
	pmPath := filepath.Join(clone, snapshot+".parts.json")
	if err := os.WriteFile(pmPath, pmBody, 0o644); err != nil {
		t.Fatalf("write parts manifest: %v", err)
	}
	if !rw.noPartsTag {
		tagKey := key
		if rw.badPartsKey {
			tagKey = bytes.Repeat([]byte{7}, len(key))
		}
		if err := backup.WriteMACFile(pmPath+".mac", tagKey, pmBody, name, filepath.Base(pmPath)); err != nil {
			t.Fatalf("tag parts manifest: %v", err)
		}
	}

	if rw.recovery != nil {
		if err := os.WriteFile(filepath.Join(clone, "RECOVERY.md"), rw.recovery, 0o644); err != nil {
			t.Fatalf("write RECOVERY.md: %v", err)
		}
	}
	if rw.beside != nil {
		rw.beside(t, clone)
	}
	runGit(t, clone, "add", "-A")
	runGit(t, clone, "commit", "--quiet", "-m", "rewrite")
	runGit(t, clone, "push", "--quiet", "--force", bare, "HEAD:refs/heads/backup")
}

// macFileBytes is a .mac tag over body under key.
func macFileBytes(t *testing.T, target string, key, body []byte, over string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tag.mac")
	if err := backup.WriteMACFile(path, key, body, target, over); err != nil {
		t.Fatalf("write tag: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tag: %v", err)
	}
	return out
}

func entryNamed(entries []tarEntry, name string) int {
	for i, e := range entries {
		if e.name == name {
			return i
		}
	}
	return -1
}

func requireNoBinary(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration")
	}
	requireTools(t, gitTools...)
	if _, err := exec.LookPath("exocomp"); err == nil {
		t.Skip("an exocomp binary is on PATH: the no-binary path cannot be exercised")
	}
}

// TestRecoveryShRestoresAGitTargetWithoutTheBinary: the card has a git chain,
// and so does the script. The snapshot is restored from a bare repository with
// no keyring flag and no target name, from one part and from several, zstd and
// gzip, with the parts manifest and MANIFEST.json authenticated.
func TestRecoveryShRestoresAGitTargetWithoutTheBinary(t *testing.T) {
	requireNoBinary(t)

	for _, c := range []struct {
		name string
		rw   *rewrite
	}{
		{"one zstd part", nil},
		{"three zstd parts", &rewrite{parts: 3}},
		{"one gzip part", &rewrite{gzip: true}},
		{"a hundred and one parts", &rewrite{parts: 101}},
	} {
		t.Run(c.name, func(t *testing.T) {
			bare, identity := backupFixtureFor(t, backup.KindGit)
			if c.rw != nil {
				rewriteSnapshot(t, bare, identity, *c.rw)
			}
			home := t.TempDir()
			dest := filepath.Join(t.TempDir(), "restored")

			out, err := runRecovery(t, home, "--from", bare, "--dest", dest, "--identity", identity)
			if err != nil {
				t.Fatalf("recovery.sh restore from git: %v\n%s", err, out)
			}

			info, err := os.Stat(dest)
			if err != nil {
				t.Fatalf("recovery.sh exited 0 but created no data dir: %v\n%s", err, out)
			}
			if got := info.Mode().Perm(); got != 0o700 {
				t.Errorf("restored data dir is %04o, want 0700", got)
			}
			for _, name := range []string{"exocomp.db", "keyring.age", "MANIFEST.json"} {
				if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
					t.Errorf("restored dir holds no %s: %v", name, err)
				}
			}
			if strings.Contains(out, "not authenticated") {
				t.Errorf("a genuine snapshot was not authenticated:\n%s", out)
			}
		})
	}
}

// TestRecoveryShGitRestoreRefusesWhatItCannotTrust: a git remote is encrypted
// to public keys, so anyone who can push can write a snapshot that decrypts.
// The script has to refuse each way such a snapshot differs from the leg's, and
// leave no data dir behind.
func TestRecoveryShGitRestoreRefusesWhatItCannotTrust(t *testing.T) {
	requireNoBinary(t)

	wrong := filepath.Join(t.TempDir(), "wrong.key")
	if err := exec.Command("age-keygen", "-o", wrong).Run(); err != nil {
		t.Skipf("age-keygen unavailable: %v", err)
	}

	cases := []struct {
		name string
		rw   *rewrite
		args []string
		want string
		// allowed is what --allow-unauthenticated does to the same snapshot:
		// empty means it restores, otherwise the refusal that remains.
		allowed  string
		useWrong bool
	}{
		{
			name:    "a parts manifest with no tag",
			rw:      &rewrite{noPartsTag: true},
			want:    "cannot be authenticated",
			allowed: "",
		},
		{
			name:    "a parts manifest tagged under another key",
			rw:      &rewrite{badPartsKey: true},
			want:    "does not authenticate",
			allowed: "does not authenticate",
		},
		{
			name: "a part changed after the manifest was written",
			rw: &rewrite{breakParts: func(p map[string][]byte) {
				for name, b := range p {
					p[name] = append([]byte{}, b...)
					p[name][len(b)/2] ^= 0xff
				}
			}},
			want:    "sha256 differs",
			allowed: "sha256 differs",
		},
		{
			name: "a MANIFEST.json with no tag",
			rw: &rewrite{mutate: func(e []tarEntry, _ []byte) []tarEntry {
				return append(e[:entryNamed(e, "MANIFEST.mac")], e[entryNamed(e, "MANIFEST.mac")+1:]...)
			}},
			want:    "cannot be authenticated",
			allowed: "",
		},
		{
			name: "a MANIFEST.json tagged under another key",
			rw: &rewrite{mutate: func(e []tarEntry, key []byte) []tarEntry {
				i := entryNamed(e, "MANIFEST.json")
				e[entryNamed(e, "MANIFEST.mac")].body = macFileBytes(t, gitTargetName, bytes.Repeat([]byte{7}, len(key)), e[i].body, "MANIFEST.json")
				return e
			}},
			want:    "does not authenticate",
			allowed: "does not authenticate",
		},
		{
			name: "a keyring older than the snapshot",
			rw: &rewrite{mutate: func(e []tarEntry, key []byte) []tarEntry {
				i := entryNamed(e, "MANIFEST.json")
				e[i].body = bytes.Replace(e[i].body, []byte(`"manifest_version"`), []byte(`"keyring_version": 9,`+"\n  "+`"manifest_version"`), 1)
				e[entryNamed(e, "MANIFEST.mac")].body = macFileBytes(t, gitTargetName, key, e[i].body, "MANIFEST.json")
				return e
			}},
			want:    "older than the snapshot",
			allowed: "older than the snapshot",
		},
		{
			name: "a symlink in the archive",
			rw: &rewrite{mutate: func(e []tarEntry, _ []byte) []tarEntry {
				return append(e, tarEntry{name: "evil", typeflag: tar.TypeSymlink, link: "/etc/passwd"})
			}},
			want:    "not a regular file",
			allowed: "not a regular file",
		},
		{
			name: "a path in the archive",
			rw: &rewrite{mutate: func(e []tarEntry, _ []byte) []tarEntry {
				return append(e, tarEntry{name: "../evil", typeflag: tar.TypeReg, body: []byte("x")})
			}},
			want:    "entry with a path",
			allowed: "entry with a path",
		},
		{
			name: "a symbolic link on the branch",
			rw: &rewrite{beside: func(t *testing.T, clone string) {
				if err := os.Symlink("/etc/hostname", filepath.Join(clone, "notes.txt")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			}},
			want:    "symbolic link",
			allowed: "symbolic link",
		},
		{
			name: "a second snapshot on the branch",
			rw: &rewrite{beside: func(t *testing.T, clone string) {
				if err := os.WriteFile(filepath.Join(clone, "snapshot-99991231T235959Z.tar.zst.age.parts.json"), []byte("{}\n"), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			}},
			want:    "keeps exactly one",
			allowed: "keeps exactly one",
		},
		{
			name:    "a parts manifest describing another snapshot",
			rw:      &rewrite{bodySnapshot: "snapshot-20200101T000000Z.tar.zst.age"},
			want:    "describes a different snapshot",
			allowed: "describes a different snapshot",
		},
		{
			name: "two archive entries of one name",
			rw: &rewrite{mutate: func(e []tarEntry, _ []byte) []tarEntry {
				return append(e, tarEntry{name: "snapshot.db", typeflag: tar.TypeReg, body: []byte("x")})
			}},
			want:    "two entries of the same name",
			allowed: "two entries of the same name",
		},
		{
			name:    "a snapshot written for another target",
			rw:      &rewrite{target: "other", recovery: []byte("target-name: other\n")},
			args:    []string{"--target-name", gitTargetName},
			want:    "does not authenticate",
			allowed: "does not authenticate",
		},
		{
			name:     "an identity that unwraps nothing",
			useWrong: true,
			want:     "cannot be authenticated",
			allowed:  "could not decrypt",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bare, identity := backupFixtureFor(t, backup.KindGit)
			if c.rw != nil {
				rewriteSnapshot(t, bare, identity, *c.rw)
			}
			if c.useWrong {
				identity = wrong
			}
			home := t.TempDir()

			dest := filepath.Join(home, "refused")
			out, err := runRecovery(t, home, append([]string{"--from", bare, "--dest", dest, "--identity", identity}, c.args...)...)
			if err == nil {
				t.Fatalf("recovery.sh exited 0:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("recovery.sh did not say why it stopped (want %q):\n%s", c.want, out)
			}
			if _, err := os.Stat(dest); err == nil {
				t.Errorf("a refused restore left %s behind", dest)
			}

			// --allow-unauthenticated waives a missing tag and nothing else.
			dest = filepath.Join(home, "allowed")
			out, err = runRecovery(t, home, append([]string{"--from", bare, "--dest", dest, "--identity", identity, "--allow-unauthenticated"}, c.args...)...)
			if c.allowed == "" {
				if err != nil {
					t.Errorf("--allow-unauthenticated did not accept a snapshot whose only fault is a missing tag: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("--allow-unauthenticated accepted the snapshot:\n%s", out)
			}
			if !strings.Contains(out, c.allowed) {
				t.Errorf("with --allow-unauthenticated, want %q:\n%s", c.allowed, out)
			}
			if _, err := os.Stat(dest); err == nil {
				t.Errorf("a refused restore left %s behind", dest)
			}
		})
	}
}

// TestRecoveryShGitRestoreRefusesAnEmptyBranch: a branch with no snapshot on it
// stops with a reason.
func TestRecoveryShGitRestoreRefusesAnEmptyBranch(t *testing.T) {
	requireNoBinary(t)

	_, identity := backupFixtureFor(t, backup.KindGit)
	empty := filepath.Join(t.TempDir(), "empty.git")
	runGit(t, "", "init", "--bare", "--quiet", empty)
	seed := filepath.Join(t.TempDir(), "seed")
	runGit(t, "", "init", "--quiet", "-b", "backup", seed)
	runGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "empty")
	runGit(t, seed, "push", "--quiet", empty, "HEAD:refs/heads/backup")

	home := t.TempDir()
	dest := filepath.Join(home, "dest")
	out, err := runRecovery(t, home, "--from", empty, "--dest", dest, "--identity", identity)
	if err == nil {
		t.Fatalf("recovery.sh exited 0:\n%s", out)
	}
	if !strings.Contains(out, "holds no snapshot") {
		t.Errorf("recovery.sh did not say why it stopped:\n%s", out)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("a failed restore left %s behind", dest)
	}
}

// TestRecoveryShGitRestoreMasksUrlCredentials: a token written into a git url
// must not reach the terminal, in the script's own lines or in git's.
func TestRecoveryShGitRestoreMasksUrlCredentials(t *testing.T) {
	requireNoBinary(t)

	_, identity := backupFixtureFor(t, backup.KindGit)
	// The second password holds an @, so the userinfo ends at the last one.
	for _, password := range []string{"tok-9f2c1d", "p@ss-9f2c1d"} {
		home := t.TempDir()
		// Nothing listens on port 1, so the clone fails fast after the script
		// has named the url.
		out, err := runRecovery(t, home, "--from", "https://user:"+password+"@127.0.0.1:1/x.git",
			"--dest", filepath.Join(home, "dest"), "--identity", identity)
		if err == nil {
			t.Fatalf("recovery.sh exited 0:\n%s", out)
		}
		if strings.Contains(out, "9f2c1d") || strings.Contains(out, "user:") {
			t.Errorf("recovery.sh printed part of the url credential %q:\n%s", password, out)
		}
		if !strings.Contains(out, "https://***@127.0.0.1:1/x.git") {
			t.Errorf("recovery.sh did not name the masked url:\n%s", out)
		}
	}
}

// TestRecoveryShGitRestorePinsTheKeyring: the tags are keyed from the keyring
// on the branch, and a remote's writer who knows a recipient's public key can
// write a keyring and snapshot that agree. A keyring the operator holds is the
// only anchor outside the remote, and the script refuses a branch that differs.
func TestRecoveryShGitRestorePinsTheKeyring(t *testing.T) {
	requireNoBinary(t)

	bare, identity := backupFixtureFor(t, backup.KindGit)
	trusted := filepath.Join(t.TempDir(), "keyring.age")
	if err := os.WriteFile(trusted, run(t, nil, "git", "-C", bare, "show", "backup:keyring.age"), 0o600); err != nil {
		t.Fatalf("save the trusted keyring: %v", err)
	}
	home := t.TempDir()

	// The branch's own keyring: restores, pinned or not, and says which.
	out, err := runRecovery(t, home, "--from", bare, "--dest", filepath.Join(home, "plain"), "--identity", identity)
	if err != nil {
		t.Fatalf("unpinned restore: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pass --keyring") {
		t.Errorf("an unpinned restore did not say the keyring is the remote's own:\n%s", out)
	}
	out, err = runRecovery(t, home, "--from", bare, "--dest", filepath.Join(home, "pinned"),
		"--identity", identity, "--keyring", trusted)
	if err != nil {
		t.Fatalf("pinned restore: %v\n%s", err, out)
	}
	if strings.Contains(out, "pass --keyring") {
		t.Errorf("a pinned restore still warned about the keyring:\n%s", out)
	}

	// A branch rewritten under a different keyring is refused when pinned.
	rewriteSnapshot(t, bare, identity, rewrite{forgeKeyring: true})
	dest := filepath.Join(home, "forged")
	out, err = runRecovery(t, home, "--from", bare, "--dest", dest, "--identity", identity, "--keyring", trusted)
	if err == nil {
		t.Fatalf("recovery.sh accepted a branch under another keyring:\n%s", out)
	}
	if !strings.Contains(out, "differs from") {
		t.Errorf("recovery.sh did not say the keyring differs:\n%s", out)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("a refused restore left %s behind", dest)
	}
}

// TestRecoveryShKeepsKeysOutOfArguments: openssl takes a key only as a command
// line argument, readable by every local user while the command runs. The DEK
// and the subkeys it derives go to openssl as data on standard input instead.
func TestRecoveryShKeepsKeysOutOfArguments(t *testing.T) {
	body, err := os.ReadFile("recovery.sh")
	if err != nil {
		t.Fatalf("read recovery.sh: %v", err)
	}
	for _, banned := range []string{"hexkey", "-macopt", "-kdfopt", "-hmac"} {
		if strings.Contains(string(body), banned) {
			t.Errorf("recovery.sh passes key material to openssl as an argument (%s)", banned)
		}
	}
}

// TestRecoveryShGitRestoreSaysWhereTheTargetNameCameFrom: the name keys the
// tags, so a name taken from the branch is flagged, and one the operator gives
// is not.
func TestRecoveryShGitRestoreSaysWhereTheTargetNameCameFrom(t *testing.T) {
	requireNoBinary(t)

	bare, identity := backupFixtureFor(t, backup.KindGit)
	home := t.TempDir()

	out, err := runRecovery(t, home, "--from", bare, "--dest", filepath.Join(home, "a"), "--identity", identity)
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if !strings.Contains(out, "comes from the backup branch itself, so it does not show which target") {
		t.Errorf("a name read from RECOVERY.md was not flagged:\n%s", out)
	}

	out, err = runRecovery(t, home, "--from", bare, "--dest", filepath.Join(home, "b"), "--identity", identity,
		"--target-name", gitTargetName)
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if strings.Contains(out, "target name") {
		t.Errorf("a name the operator gave was flagged:\n%s", out)
	}
}

// TestRecoveryShGitRestoreKeepsTheTerminalClean: names and values from the
// remote reach the script's messages, and a control character in one must not
// redraw the terminal or forge a line.
func TestRecoveryShGitRestoreKeepsTheTerminalClean(t *testing.T) {
	requireNoBinary(t)

	bare, identity := backupFixtureFor(t, backup.KindGit)
	rewriteSnapshot(t, bare, identity, rewrite{
		target:   "x",
		recovery: []byte("target-name: \x1b[2Jevil\nrecovery.sh: restored everything\n"),
	})
	home := t.TempDir()
	out, err := runRecovery(t, home, "--from", bare, "--dest", filepath.Join(home, "d"), "--identity", identity)
	if err == nil {
		t.Fatalf("recovery.sh exited 0:\n%s", out)
	}
	if strings.ContainsAny(out, "\x1b\r\x07") {
		t.Errorf("recovery.sh passed a control character through:\n%q", out)
	}
	if strings.Contains(out, "\nrecovery.sh: restored everything") {
		t.Errorf("a value from the remote forged a line of output:\n%s", out)
	}
}

// TestRecoveryShPinNeedsTheChainRunHere: exocomp restore cannot pin a keyring,
// so with --keyring the script runs its own chain even when a binary is on
// PATH, and without it still hands the restore to the binary.
func TestRecoveryShPinNeedsTheChainRunHere(t *testing.T) {
	requireNoBinary(t)

	bare, identity := backupFixtureFor(t, backup.KindGit)
	trusted := filepath.Join(t.TempDir(), "keyring.age")
	if err := os.WriteFile(trusted, run(t, nil, "git", "-C", bare, "show", "backup:keyring.age"), 0o600); err != nil {
		t.Fatalf("save the trusted keyring: %v", err)
	}
	stubDir := t.TempDir()
	marker := filepath.Join(stubDir, "ran")
	stub := "#!/bin/sh\necho stub >> '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "exocomp"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	withStub := func(args ...string) (string, error) {
		cmd := exec.Command("sh", append([]string{"recovery.sh"}, args...)...)
		cmd.Env = []string{
			"PATH=" + stubDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			"TMPDIR=" + t.TempDir(),
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	home := t.TempDir()

	out, err := withStub("--from", bare, "--dest", filepath.Join(home, "pinned"), "--identity", identity, "--keyring", trusted)
	if err != nil {
		t.Fatalf("pinned restore with a binary on PATH: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("the binary ran although a keyring pin was given:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "pinned", "exocomp.db")); err != nil {
		t.Errorf("the pinned restore restored nothing: %v\n%s", err, out)
	}

	if out, err = withStub("--from", bare, "--dest", filepath.Join(home, "plain"), "--identity", identity); err != nil {
		t.Fatalf("restore with a binary on PATH: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("an unpinned restore did not go to the binary:\n%s", out)
	}
}

// shellFunctions returns the named top-level functions of recovery.sh, so a
// test can run them alone.
func shellFunctions(t *testing.T, names ...string) string {
	t.Helper()
	body, err := os.ReadFile("recovery.sh")
	if err != nil {
		t.Fatalf("read recovery.sh: %v", err)
	}
	var out []string
	lines := strings.Split(string(body), "\n")
	for _, name := range names {
		found := false
		for i, line := range lines {
			if line != name+"() {" {
				continue
			}
			found = true
			for j := i; j < len(lines); j++ {
				out = append(out, lines[j])
				if lines[j] == "}" {
					break
				}
			}
			break
		}
		if !found {
			t.Fatalf("recovery.sh has no function %s", name)
		}
	}
	return strings.Join(out, "\n")
}

// TestRecoveryShClassifiesTargetsAsRestoreDoes: with --keyring set, a source
// that exocomp restore would read as git must never reach it, so the script's
// is_git_target has to agree with ParseSource on every spelling, whitespace and
// local: forms included.
func TestRecoveryShClassifiesTargetsAsRestoreDoes(t *testing.T) {
	requireTools(t, "sh", "sed", "printf")

	dir := t.TempDir()
	withObjects := filepath.Join(dir, "bare")
	if err := os.MkdirAll(filepath.Join(withObjects, "objects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	without := filepath.Join(dir, "restic-repo")
	if err := os.MkdirAll(filepath.Join(without, "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	specs := []string{
		withObjects, "local:" + withObjects, "  " + withObjects + "\t", "restic_local:" + withObjects,
		"git:" + withObjects, "git:", "git:   ", "  git:/srv/x  ",
		without, "local:" + without, "restic_local:" + without, "restic_local:", "restic_s3:s3:bucket",
		"s3:https://host/bucket", "s3:https://host/bucket.git",
		"/srv/x.git", "local:/srv/x.git", "restic_local:/srv/x.git", "  /srv/x.git  ", "x.git ",
		"git@host:org/repo", "git@host:org/repo.git", "host.example:org/repo", "user@host:org/repo",
		"git://host/repo", "ssh://host/repo", "ssh://user@host/repo.git", "https://host/repo",
		"http://host/repo", "http://host/repo.git", "file:///srv/repo", "file:///srv/repo.git",
		"file://" + withObjects, "ftp://host/repo.git", "", "   ", "/nonexistent",
	}
	funcs := shellFunctions(t, "trim", "is_git_target")
	for _, spec := range specs {
		src, err := backup.ParseSource(spec)
		if err != nil {
			continue
		}
		want := src.Kind == backup.KindGit

		cmd := exec.Command("sh", "-c", funcs+"\nis_git_target \"$1\"", "sh", spec)
		got := cmd.Run() == nil
		if got != want {
			t.Errorf("is_git_target(%q) = %v, ParseSource says kind %s", spec, got, src.Kind)
		}
	}
}

// TestRecoveryShOutputIsInert: a name from the remote reaches the script's
// messages, and neither an escape sequence nor a backslash sequence in it may
// reach the terminal. dash's echo reads backslash sequences, and bash's does
// with xpg_echo, so the test runs under dash when there is one.
func TestRecoveryShOutputIsInert(t *testing.T) {
	requireNoBinary(t)
	shell := "sh"
	if p, err := exec.LookPath("dash"); err == nil {
		shell = p
	}

	bare, identity := backupFixtureFor(t, backup.KindGit)
	hostile := "\x1b[2J\\033[31m\\n\\c\\0 \x07 \u00e9 end"
	rewriteSnapshot(t, bare, identity, rewrite{
		target:   "x",
		recovery: []byte("target-name: " + hostile + "\n"),
	})
	home := t.TempDir()
	cmd := exec.Command(shell, "recovery.sh", "--from", bare, "--dest", filepath.Join(home, "d"), "--identity", identity)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + t.TempDir()}
	raw, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("recovery.sh exited 0:\n%s", raw)
	}
	out := string(raw)
	for _, r := range strings.ReplaceAll(out, "\n", "") {
		if r < ' ' || r > '~' {
			t.Fatalf("recovery.sh printed %q:\n%q", r, out)
		}
	}
	// The text survives as inert characters, so the operator can still see
	// what the remote supplied.
	if !strings.Contains(out, `\033[31m\n\c\0`) {
		t.Errorf("the backslash text was altered:\n%s", out)
	}
}
