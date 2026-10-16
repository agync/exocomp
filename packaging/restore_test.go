// Package packaging holds the separately versioned recovery script. It carries
// no Go code; this test drives recovery.sh, because the no-binary restore path
// is the one piece of disaster recovery that cannot be checked by running
// exocomp.
package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

const sealedValue = "install-sh-restore-proof-4c1a"

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not on PATH: the manual restore path cannot be exercised", n)
		}
	}
}

// backupFixture builds a real restic_local target with one snapshot in it and
// returns the repository path and the recovery identity file.
func backupFixture(t *testing.T) (repo, identity string) {
	t.Helper()

	dataDir := filepath.Join(t.TempDir(), "data")
	env, kit, err := secrets.Init(dataDir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })

	identity = filepath.Join(t.TempDir(), "recovery.key")
	if err := os.WriteFile(identity, []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write recovery identity: %v", err)
	}

	st, err := store.Open(filepath.Join(dataDir, "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sealed, err := env.Seal("proof", "e2e", []byte(sealedValue))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := st.CreateSecret(t.Context(), store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce,
		Ciphertext: sealed.Ciphertext, AAD: sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		t.Fatalf("create secret row: %v", err)
	}

	repo = filepath.Join(t.TempDir(), "repo")
	row, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "nas", Kind: backup.KindResticLocal, URI: repo, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	cache := t.TempDir()
	report, err := backup.New(st, env, backup.Options{}).Run(t.Context(), []backup.Target{
		{BackupTarget: row, Env: map[string]string{"RESTIC_CACHE_DIR": cache}},
	})
	if err != nil {
		t.Fatalf("backup run: %v", err)
	}
	if err := report.Err(); err != nil {
		t.Fatalf("backup leg failed: %v", err)
	}
	return repo, identity
}

// runRecovery runs recovery.sh with no exocomp binary reachable: neither
// EXOCOMP_BIN nor anything named exocomp on PATH.
func runRecovery(t *testing.T, home string, args ...string) (string, error) {
	t.Helper()

	cmd := exec.Command("sh", append([]string{"recovery.sh"}, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + t.TempDir(),
		"RESTIC_CACHE_DIR=" + filepath.Join(home, "restic-cache"),
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestRecoveryShRestoresWithoutTheBinary is the claim the recovery script makes:
// an operator who has the recovery key and the target, and nothing else, gets
// their data dir back. It replaced an installer that printed "would restore" and
// exited 0.
func TestRecoveryShRestoresWithoutTheBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	requireTools(t, "sh", "age", "openssl", "restic", "od", "tr", "base64", "sha256sum", "awk")
	if _, err := exec.LookPath("exocomp"); err == nil {
		t.Skip("an exocomp binary is on PATH: the no-binary path cannot be exercised")
	}

	repo, identity := backupFixture(t)
	home := t.TempDir()
	dest := filepath.Join(t.TempDir(), "restored")

	out, err := runRecovery(t, home, "--from", repo, "--dest", dest, "--identity", identity)
	if err != nil {
		t.Fatalf("recovery.sh restore: %v\n%s", err, out)
	}
	if strings.Contains(out, "would restore") {
		t.Fatalf("recovery.sh reported success without restoring:\n%s", out)
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

	// The restored database has to be the real one, not an empty file that
	// happens to be in the right place.
	st, err := store.Open(filepath.Join(dest, "exocomp.db"))
	if err != nil {
		t.Fatalf("restored database does not open: %v", err)
	}
	defer func() { _ = st.Close() }()

	row, err := st.GetSecret(t.Context(), "proof")
	if err != nil {
		t.Fatalf("restored database has no sealed secret: %v", err)
	}
	if len(row.Ciphertext) == 0 {
		t.Error("the restored secret row carries no ciphertext")
	}
	if strings.Contains(out, sealedValue) {
		t.Error("recovery.sh printed the sealed plaintext")
	}
}

func TestRecoveryShRestoreFailsLoudly(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	requireTools(t, "sh", "age", "openssl", "restic")
	if _, err := exec.LookPath("exocomp"); err == nil {
		t.Skip("an exocomp binary is on PATH: the no-binary path cannot be exercised")
	}

	repo, identity := backupFixture(t)
	home := t.TempDir()

	wrong := filepath.Join(t.TempDir(), "wrong.key")
	gen := exec.Command("age-keygen", "-o", wrong)
	if err := gen.Run(); err != nil {
		t.Skipf("age-keygen unavailable: %v", err)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "an identity that unwraps nothing",
			args: []string{"--from", repo, "--dest", filepath.Join(home, "a"), "--identity", wrong},
			want: "could not unwrap",
		},
		{
			name: "no identity and no terminal to ask at",
			args: []string{"--from", repo, "--dest", filepath.Join(home, "b")},
			want: "requires --identity",
		},
		{
			name: "a target that is not a directory",
			args: []string{"--from", "s3:https://nas.invalid/bucket",
				"--dest", filepath.Join(home, "c"), "--identity", identity},
			want: "--keyring",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := runRecovery(t, home, c.args...)
			if err == nil {
				t.Fatalf("recovery.sh exited 0:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("recovery.sh did not say why it stopped (want %q):\n%s", c.want, out)
			}
			dest := c.args[len(c.args)-1]
			if strings.HasPrefix(dest, home) {
				if _, err := os.Stat(dest); err == nil {
					t.Errorf("a failed restore left %s behind", dest)
				}
			}
		})
	}
}

// TestRecoveryShImageFlags pins the two disaster-recovery image flags the
// container docs describe: --image-load refuses a missing archive before any
// restore, and --image-ref prints the unit's Environment= line so the operator
// can set EXOCOMP_IMAGE= on the new host (Decision 0016).
func TestRecoveryShImageFlags(t *testing.T) {
	requireTools(t, "sh")
	home := t.TempDir()

	// --image-load refuses an archive that is not there, before a target is
	// ever opened.
	out, err := runRecovery(t, home, "--image-load", filepath.Join(home, "missing.tar"))
	if err == nil {
		t.Fatalf("--image-load on a missing archive exited 0:\n%s", out)
	}
	if !strings.Contains(out, "is not a file") {
		t.Errorf("--image-load did not name the missing archive:\n%s", out)
	}

	// --image-ref prints the exact unit line, with or without a restore to
	// follow (no --from here, so the run stops right after the echo).
	const ref = "ghcr.io/abn/exocomp:0.1.0"
	out, _ = runRecovery(t, home, "--image-ref", ref)
	if want := "Environment=EXOCOMP_IMAGE=" + ref; !strings.Contains(out, want) {
		t.Errorf("--image-ref did not print %q:\n%s", want, out)
	}

	help, err := runRecovery(t, home, "--help")
	if err != nil {
		t.Fatalf("recovery.sh --help: %v\n%s", err, help)
	}
	for _, flag := range []string{"--image-load", "--image-ref"} {
		if !strings.Contains(help, flag) {
			t.Errorf("recovery.sh --help does not list %s:\n%s", flag, help)
		}
	}
}
