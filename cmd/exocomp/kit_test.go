package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// initForKit initializes a scratch data dir the way `exocomp init` does and
// returns the dir plus the recovery identity file the offline path needs.
func initForKit(t *testing.T) (dir, identity string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	dir = filepath.Join(root, "exocomp")

	env, kit, err := secrets.Init(dir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	defer func() { _ = env.Close() }()

	identity = filepath.Join(t.TempDir(), "recovery.key")
	if err := os.WriteFile(identity, []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write recovery identity: %v", err)
	}
	return dir, identity
}

func runKit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newKitCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestKitDeriveMatchesTheEnvelope checks the CLI against the envelope for three
// purposes, online and offline. Offline is the case that matters: a dead
// daemon must not brick the operator's own playbooks.
func TestKitDeriveMatchesTheEnvelope(t *testing.T) {
	dir, identity := initForKit(t)

	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	defer func() { _ = env.Close() }()

	for _, tc := range []struct{ arg, purpose, id string }{
		{"restic/nas", "restic-repo", "nas"},
		{"ansible-vault/homelab", "ansible-vault", "homelab"},
		{"tofu-state/edge", "tofu-state", "edge"},
	} {
		want := base64.StdEncoding.EncodeToString(env.Derive(tc.purpose, tc.id))

		online, _, err := runKit(t, "derive", tc.arg)
		if err != nil {
			t.Fatalf("kit derive %s: %v", tc.arg, err)
		}
		if got := strings.TrimSpace(online); got != want {
			t.Errorf("kit derive %s = %q, envelope derives %q", tc.arg, got, want)
		}

		offline, _, err := runKit(t, "derive", tc.arg,
			"--identity", identity, "--keyring", filepath.Join(dir, secrets.KeyringName))
		if err != nil {
			t.Fatalf("kit derive %s --identity: %v", tc.arg, err)
		}
		if got := strings.TrimSpace(offline); got != want {
			t.Errorf("offline kit derive %s = %q, envelope derives %q", tc.arg, got, want)
		}
	}

	if _, _, err := runKit(t, "derive", "restic-repo:nas"); err == nil {
		t.Error("kit derive accepted an argument that is not <purpose>/<id>")
	}
	if _, _, err := runKit(t, "derive", "sops-age/homelab"); err == nil {
		t.Error("kit derive accepted a purpose the card does not list")
	}
}

// TestKitDeriveOpensARealResticRepo is the cross-check that matters for the
// restic purpose: a repository created with the derived password opens with it
// and with nothing else.
func TestKitDeriveOpensARealResticRepo(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not on PATH: cross-tool proof skipped")
	}
	dir, identity := initForKit(t)

	password, _, err := runKit(t, "derive", "restic/nas", "--identity", identity,
		"--keyring", filepath.Join(dir, secrets.KeyringName))
	if err != nil {
		t.Fatalf("kit derive restic/nas: %v", err)
	}
	password = strings.TrimSpace(password)

	repo := filepath.Join(t.TempDir(), "repo")
	base := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"RESTIC_CACHE_DIR=" + t.TempDir(),
	}
	for _, args := range [][]string{{"init"}, {"cat", "config"}} {
		cmd := exec.Command("restic", append([]string{"-r", repo}, args...)...)
		cmd.Env = append(slices.Clone(base), "RESTIC_PASSWORD="+password)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restic %s with the derived password: %v\n%s", args[0], err, out)
		}
	}

	wrong := exec.Command("restic", "-r", repo, "cat", "config")
	wrong.Env = append(slices.Clone(base), "RESTIC_PASSWORD="+password+"x")
	if err := wrong.Run(); err == nil {
		t.Fatal("restic opened the repository with a password kit derive did not produce")
	}
}

func TestKitDeriveListsWhatIsInUse(t *testing.T) {
	initForKit(t)

	st, err := openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if _, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "nas", Kind: "restic_local", URI: t.TempDir(), Enabled: true,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := st.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "offsite", Kind: "git", URI: "/srv/offsite.git", Enabled: true,
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := st.CreateWorkspace(t.Context(), store.Workspace{
		Name: "homelab", TofuDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := st.CreateJob(t.Context(), store.NewJob{
		Runbook: "playbook:site.yml", Kind: "ansible", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"ansible-playbook", "site.yml"}, VaultProject: "homelab"},
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	out, _, err := runKit(t, "derive", "--list")
	if err != nil {
		t.Fatalf("kit derive --list: %v", err)
	}
	for _, want := range []string{
		"restic-repo:nas",
		"backup-manifest-mac:nas",
		"backup-manifest-mac:offsite",
		"tofu-state:homelab",
		"tf-backend:homelab",
		"ansible-vault:homelab",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("kit derive --list omits %s:\n%s", want, out)
		}
	}
	// A git target has no restic repository, so it has no repository password.
	if strings.Contains(out, "restic-repo:offsite") {
		t.Errorf("kit derive --list invented a restic password for a git target:\n%s", out)
	}
	if _, _, err := runKit(t, "derive", "--list", "restic/nas"); err == nil {
		t.Error("kit derive accepted --list together with a purpose")
	}
}

// TestKitVerifyRoundTripsAndCatchesAnEdit is the quarterly check the card tells
// the operator to run: the printed page still says what the daemon says.
func TestKitVerifyRoundTripsAndCatchesAnEdit(t *testing.T) {
	dir, _ := initForKit(t)

	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	kit := env.KitStructure()
	decorateKit(t.Context(), kit, os.Stderr)
	page := kit.Render()
	if err := env.Close(); err != nil {
		t.Fatalf("close envelope: %v", err)
	}

	printed := filepath.Join(t.TempDir(), "kit.txt")
	if err := os.WriteFile(printed, []byte(page), 0o600); err != nil {
		t.Fatalf("write page: %v", err)
	}

	out, _, err := runKit(t, "verify", "--page", printed)
	if err != nil {
		t.Fatalf("kit verify on the page it just rendered: %v", err)
	}
	if !strings.Contains(out, "page matches the current kit") {
		t.Errorf("kit verify said %q", out)
	}

	// One flipped byte in the body: the page no longer hashes to what it prints.
	edited := []byte(page)
	edited[strings.Index(page, "3 · SALT")+4] ^= 0x20
	if err := os.WriteFile(printed, edited, 0o600); err != nil {
		t.Fatalf("write edited page: %v", err)
	}
	_, _, err = runKit(t, "verify", "--page", printed)
	if err == nil {
		t.Fatal("kit verify accepted a page with a flipped byte")
	}
	if !strings.Contains(err.Error(), "edited after it was printed") {
		t.Errorf("kit verify reported %v", err)
	}

	// A kit that moved on: the page is intact, and stale.
	if _, err := secrets.Migrate(dir, secrets.MigrateOptions{RotateHost: true}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := os.WriteFile(printed, []byte(page), 0o600); err != nil {
		t.Fatalf("rewrite page: %v", err)
	}
	_, _, err = runKit(t, "verify", "--page", printed)
	if err == nil {
		t.Fatal("kit verify accepted a page from an older keyring generation")
	}
	if !strings.Contains(err.Error(), "reprint") {
		t.Errorf("kit verify reported %v", err)
	}
}

// TestIdentityMigrateRoundTrip drives the CLI through add, drop and host
// rotation, and checks the nag the operator has to act on.
func TestIdentityMigrateRoundTrip(t *testing.T) {
	dir, identity := initForKit(t)

	before, _, err := runKit(t, "derive", "restic/nas")
	if err != nil {
		t.Fatalf("kit derive: %v", err)
	}

	laptopKey := filepath.Join(t.TempDir(), "laptop.key")
	laptop := generateIdentity(t, laptopKey)

	run := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		cmd := newIdentityCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("identity %s: %v (%s)", strings.Join(args, " "), err, errOut.String())
		}
		return out.String()
	}

	added := run("migrate", "--add-recipient", "laptop="+laptop)
	if !strings.Contains(added, "added    laptop") {
		t.Errorf("migrate did not report the added recipient: %q", added)
	}
	if !strings.Contains(added, "keyring is now v2, replacing v1") {
		t.Errorf("migrate did not nag about the stale page: %q", added)
	}

	// The laptop key now unwraps the keyring, and derives what the host does.
	viaLaptop, _, err := runKit(t, "derive", "restic/nas", "--identity", laptopKey,
		"--keyring", filepath.Join(dir, secrets.KeyringName))
	if err != nil {
		t.Fatalf("kit derive via the added recipient: %v", err)
	}
	if viaLaptop != before {
		t.Error("the added recipient derives a different restic password")
	}

	// Dropping is a revocation: it rotates the DEK (finding E2), so the report
	// says "revoked" and names the rotation, and every derived key changes.
	dropped := run("migrate", "--drop-recipient", "laptop", "--rotate-host")
	if !strings.Contains(dropped, "revoked  laptop") || !strings.Contains(dropped, "host identity rotated") {
		t.Errorf("migrate did not report the revocation and the rotation: %q", dropped)
	}
	if !strings.Contains(dropped, "DEK was rotated") {
		t.Errorf("migrate did not name the DEK re-key follow-through: %q", dropped)
	}
	if _, _, err := runKit(t, "derive", "restic/nas", "--identity", laptopKey,
		"--keyring", filepath.Join(dir, secrets.KeyringName)); err == nil {
		t.Fatal("the dropped recipient still unwraps the keyring")
	}

	// The DEK rotated, so the derived restic password MUST change — that is what
	// makes the revocation real. Host and printed card, still recipients of the
	// new keyring, both derive the SAME new value.
	after, _, err := runKit(t, "derive", "restic/nas")
	if err != nil {
		t.Fatalf("kit derive after rotation: %v", err)
	}
	if after == before {
		t.Error("dropping a recipient did not rotate the DEK: the derived restic password is unchanged")
	}
	viaCard, _, err := runKit(t, "derive", "restic/nas", "--identity", identity,
		"--keyring", filepath.Join(dir, secrets.KeyringName))
	if err != nil {
		t.Fatalf("kit derive with the printed recovery key: %v", err)
	}
	if viaCard != after {
		t.Error("the printed recovery key does not derive the rotated restic password")
	}
}

// TestIdentityMigrateRefusesWhileTheDaemonRuns: the daemon holds the keyring it
// started with, so a migration behind its back would be undone by the next
// backup writing that keyring back to every target root.
func TestIdentityMigrateRefusesWhileTheDaemonRuns(t *testing.T) {
	dir, _ := initForKit(t)

	lock, err := acquireServeLock(dir)
	if err != nil {
		t.Fatalf("acquire serve lock: %v", err)
	}
	defer lock.release()

	var out, errOut bytes.Buffer
	cmd := newIdentityCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"migrate", "--rotate-host"})
	err = cmd.Execute()
	if err == nil {
		t.Fatal("identity migrate ran while the daemon held the lock")
	}
	if !strings.Contains(err.Error(), "exocomp serve") {
		t.Errorf("refusal does not name the daemon: %v", err)
	}
	version, err := secrets.ReadKeyringVersion(dir)
	if err != nil {
		t.Fatalf("ReadKeyringVersion: %v", err)
	}
	if version.Version != 1 {
		t.Errorf("the refused migration bumped the keyring to %s", version)
	}
}

// generateIdentity writes a fresh age identity to path and returns its public
// recipient.
func generateIdentity(t *testing.T, path string) string {
	t.Helper()
	env, kit, err := secrets.Init(filepath.Join(t.TempDir(), "scratch"), nil)
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	defer func() { _ = env.Close() }()
	if err := os.WriteFile(path, []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	for _, r := range kit.Recipients {
		if r.Label == "recovery" {
			return r.Public
		}
	}
	t.Fatal("scratch keyring has no recovery recipient")
	return ""
}

// runIdentity executes `identity <args>` against the ambient data dir.
func runIdentity(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newIdentityCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err != nil {
		return out.String() + errOut.String(), err
	}
	return out.String(), nil
}

// sealSecretForTest seals value under name into the store the ambient data dir
// holds, the way `exocomp secrets add` would.
func sealSecretForTest(t *testing.T, dir, name, value string) {
	t.Helper()
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()
	sealed, err := env.Seal(name, "generic", []byte(value))
	if err != nil {
		t.Fatalf("seal %s: %v", name, err)
	}
	st, err := openStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.CreateSecret(t.Context(), store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce,
		Ciphertext: sealed.Ciphertext, AAD: sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		t.Fatalf("create secret %s: %v", name, err)
	}
}

// TestIdentityMigrateDropRevokesViaRotation is the E2 red-team repro: a dropped
// recipient that kept a copy of keyring.age from before the drop must not be
// able to decrypt anything — neither a secret sealed before the drop (it is
// re-encrypted under the new DEK) nor one sealed after. Before the fix, drop
// re-wrapped the SAME DEK, so the retained keyring kept decrypting everything.
func TestIdentityMigrateDropRevokesViaRotation(t *testing.T) {
	dir, _ := initForKit(t)

	victimKey := filepath.Join(t.TempDir(), "victim.key")
	victimPub := generateIdentity(t, victimKey)
	if _, err := runIdentity(t, "migrate", "--add-recipient", "victim="+victimPub); err != nil {
		t.Fatalf("add victim: %v", err)
	}

	// The victim keeps a copy of keyring.age as it stands now — the crux of the
	// attack: revocation must not depend on the dropped party discarding it.
	retained := filepath.Join(t.TempDir(), "retained.age")
	keyringBytes, err := os.ReadFile(filepath.Join(dir, secrets.KeyringName))
	if err != nil {
		t.Fatalf("read keyring: %v", err)
	}
	if err := os.WriteFile(retained, keyringBytes, 0o600); err != nil {
		t.Fatalf("retain keyring: %v", err)
	}

	sealSecretForTest(t, dir, "s1", "pre-drop value")

	if out, err := runIdentity(t, "migrate", "--drop-recipient", "victim"); err != nil {
		t.Fatalf("drop victim: %v (%s)", err, out)
	}

	sealSecretForTest(t, dir, "s2", "post-drop value")

	// The victim unwraps its retained keyring to whatever DEK it holds, and
	// tries to open the current secret rows offline.
	victimIDs, err := backup.ReadIdentities(victimKey)
	if err != nil {
		t.Fatalf("read victim identity: %v", err)
	}
	dek, err := backup.UnwrapDEK(keyringBytes, victimIDs)
	if err != nil {
		t.Fatalf("victim unwrap retained keyring: %v", err)
	}
	defer clear(dek)

	st, err := openStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	for _, name := range []string{"s1", "s2"} {
		row, err := st.GetSecret(t.Context(), name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if _, err := backup.OpenSealed(dek, row.Name, row.Kind, row.Nonce, row.Ciphertext); err == nil {
			t.Fatalf("dropped recipient decrypted %q with its retained keyring", name)
		}
	}
}
