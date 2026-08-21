package secrets

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not on PATH: cross-tool proof skipped", n)
		}
	}
}

func TestKitRendersSecretKeyOnlyAtInit(t *testing.T) {
	env, kit := mustInit(t, t.TempDir(), nil)

	minted := kit.Render()
	if !strings.Contains(minted, kit.AgeSecretKey) {
		t.Error("init kit omits the recovery secret key")
	}
	if !strings.Contains(minted, Salt) {
		t.Errorf("init kit omits the salt %q", Salt)
	}
	for _, r := range kit.Recipients {
		if !strings.Contains(minted, r.Fingerprint) {
			t.Errorf("init kit omits fingerprint of %s", r.Label)
		}
		if !strings.Contains(minted, r.Public) {
			t.Errorf("init kit omits public key of %s", r.Label)
		}
	}
	for _, phrase := range []string{"HKDF-SHA256", "age -d -i recovery.key keyring.age", "restic"} {
		if !strings.Contains(minted, phrase) {
			t.Errorf("init kit omits %q", phrase)
		}
	}
	// The card must not promise what it does not carry, and must not name a
	// flag the binary does not have.
	for _, phrase := range []string{"credential from this card", "--deep"} {
		if strings.Contains(minted, phrase) {
			t.Errorf("init kit still claims %q", phrase)
		}
	}
	if !strings.Contains(minted, "carries no storage credential") {
		t.Error("init kit does not say the storage credential is the operator's to supply")
	}

	structure := env.KitStructure().Render()
	if strings.Contains(structure, kit.AgeSecretKey) {
		t.Fatal("recovery-kit re-render leaked the recovery secret key")
	}
	if !strings.Contains(structure, "NOT shown here") {
		t.Error("recovery-kit re-render does not say the secret key is unavailable")
	}
	for _, r := range kit.Recipients {
		if !strings.Contains(structure, r.Fingerprint) {
			t.Errorf("recovery-kit re-render omits fingerprint of %s", r.Label)
		}
	}
	if !strings.Contains(structure, Salt) {
		t.Error("recovery-kit re-render omits the salt")
	}
}

func TestFingerprintIsStableAndDistinct(t *testing.T) {
	a := fingerprint("age1exampleexampleexampleexampleexampleexampleexampleexample")
	b := fingerprint("age1anotheranotheranotheranotheranotheranotheranotheranother")

	if a != fingerprint("age1exampleexampleexampleexampleexampleexampleexampleexample") {
		t.Error("fingerprint is not deterministic")
	}
	if a == b {
		t.Error("distinct recipients share a fingerprint")
	}
	if len(a) != 19 || strings.Count(a, "-") != 3 {
		t.Errorf("fingerprint %q is not four hex groups", a)
	}
}

// TestKeyringDecryptsWithAgeCLI is the cross-tool proof: the keyring must open
// with the standalone age binary, because the recovery path runs without any
// exocomp code.
func TestKeyringDecryptsWithAgeCLI(t *testing.T) {
	requireTools(t, "age")

	dir := t.TempDir()
	env, kit := mustInit(t, dir, nil)

	keyring, err := env.KeyringAge()
	if err != nil {
		t.Fatalf("KeyringAge: %v", err)
	}
	keyringPath := filepath.Join(dir, "published-keyring.age")
	if err := os.WriteFile(keyringPath, keyring, 0o644); err != nil {
		t.Fatalf("write keyring: %v", err)
	}

	recoveryPath := filepath.Join(dir, "recovery.key")
	if err := os.WriteFile(recoveryPath, []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write recovery key: %v", err)
	}

	for name, identity := range map[string]string{
		"recovery identity": recoveryPath,
		"host identity":     filepath.Join(dir, hostKeyFile),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command("age", "-d", "-i", identity, keyringPath).Output()
			if err != nil {
				t.Fatalf("age -d: %v", err)
			}
			if len(out) != dekLen {
				t.Fatalf("age CLI returned %d bytes, want %d", len(out), dekLen)
			}
			if !bytes.Equal(out, env.dek) {
				t.Fatal("age CLI decrypted a key that differs from the envelope DEK")
			}
		})
	}
}

// TestRecoveryCardRunsAsWritten executes the card's commands verbatim against a
// real restic repository: unwrap the DEK with age, derive the password with
// openssl, then restore a snapshot out of a repository this test created with
// the password Derive produces. The card's command text is a build artifact —
// a card whose commands do not run is worse than no card.
func TestRecoveryCardRunsAsWritten(t *testing.T) {
	requireTools(t, "sh", "age", "openssl", "od", "tr", "base64", "printf", "restic")

	dir := t.TempDir()
	env, kit := mustInit(t, dir, nil)

	if err := os.WriteFile(filepath.Join(dir, "recovery.key"), []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write recovery key: %v", err)
	}
	keyring, err := env.KeyringAge()
	if err != nil {
		t.Fatalf("KeyringAge: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keyring.age"), keyring, 0o644); err != nil {
		t.Fatalf("write keyring: %v", err)
	}

	const target = "nas"
	want := base64.StdEncoding.EncodeToString(env.Derive("restic-repo", target))

	// The fixture repository is created with the password Derive produces, so
	// the card's own restore step only succeeds if its openssl pipeline lands
	// on the same bytes and restic accepts the file the card writes.
	repo := filepath.Join(dir, "repo")
	restic := resticFixture(t, dir, repo, want)

	restored := filepath.Join(dir, "restored")
	script := strings.NewReplacer(
		"<target-id>", target,
		"<target-uri>", repo,
		"/srv/exocomp", restored,
	).Replace(cardCommands(t, kit.Render()))

	cmd := exec.Command("sh", "-eu", "-c", script)
	cmd.Dir = dir
	cmd.Env = restic.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("card commands failed: %v\nscript:\n%s\noutput:\n%s", err, script, out)
	}
	t.Logf("card commands ran verbatim against %s:\n%s", repo, out)

	unwrapped, err := os.ReadFile(filepath.Join(dir, "dek.bin"))
	if err != nil {
		t.Fatalf("read dek.bin: %v", err)
	}
	if !bytes.Equal(unwrapped, env.dek) {
		t.Fatal("card step 2 unwrapped a key that differs from the envelope DEK")
	}

	pass, err := os.ReadFile(filepath.Join(dir, "restic.pass"))
	if err != nil {
		t.Fatalf("read restic.pass: %v", err)
	}
	if got := strings.TrimSpace(string(pass)); got != want {
		t.Errorf("card derived a restic password that differs from Derive(%q, %q)",
			"restic-repo", target)
	}

	body, err := os.ReadFile(filepath.Join(restored, strings.TrimPrefix(restic.payload, "/")))
	if err != nil {
		t.Fatalf("card step 4 restored no %s: %v", restic.payload, err)
	}
	if string(body) != resticPayloadBody {
		t.Errorf("restored payload = %q, want %q", body, resticPayloadBody)
	}

	// A repository password the card cannot reproduce must fail this test.
	probe := exec.Command("restic", "-r", repo, "cat", "config")
	probe.Env = append(slices.Clone(restic.env), "RESTIC_PASSWORD=not-the-derived-password")
	if err := probe.Run(); err == nil {
		t.Fatal("restic opened the fixture repository with a wrong password")
	}
}

const resticPayloadBody = "recovery-card-fixture\n"

type resticRepoFixture struct {
	env []string
	// payload is the absolute path restic recorded, which is also the path it
	// recreates under --target.
	payload string
}

// resticFixture builds a restic repository holding one snapshot, locked with
// password. It stands in for a repository exocomp wrote: the card has to open
// it with nothing but age, openssl and coreutils.
func resticFixture(t *testing.T, home, repo, password string) resticRepoFixture {
	t.Helper()

	// An explicit environment, so a RESTIC_* variable exported by whoever runs
	// the suite cannot decide the outcome.
	base := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"RESTIC_CACHE_DIR=" + filepath.Join(home, "restic-cache"),
	}

	payloadDir := filepath.Join(home, "payload")
	if err := os.MkdirAll(payloadDir, 0o700); err != nil {
		t.Fatalf("create payload dir: %v", err)
	}
	payload := filepath.Join(payloadDir, "snapshot.db")
	if err := os.WriteFile(payload, []byte(resticPayloadBody), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	for _, step := range [][]string{
		{"-r", repo, "init"},
		{"-r", repo, "backup", "--quiet", payload},
	} {
		cmd := exec.Command("restic", step...)
		cmd.Env = append(slices.Clone(base), "RESTIC_PASSWORD="+password)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restic %s: %v\n%s", strings.Join(step, " "), err, out)
		}
	}
	return resticRepoFixture{env: base, payload: payload}
}

// cardCommands lifts the executable lines out of the rendered card: the age
// unwrap, the openssl derivation, and the restic restore, dedented for sh.
func cardCommands(t *testing.T, card string) string {
	t.Helper()

	var out []string
	for _, line := range strings.Split(card, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "age -d"),
			strings.HasPrefix(trimmed, "hex()"),
			strings.HasPrefix(trimmed, "openssl kdf"),
			strings.HasPrefix(trimmed, "-kdfopt"),
			strings.HasPrefix(trimmed, "-binary HKDF"),
			strings.HasPrefix(trimmed, "RESTIC_PASSWORD_FILE="),
			strings.HasPrefix(trimmed, "restore latest"):
			out = append(out, trimmed)
		}
	}
	if len(out) < 9 {
		t.Fatalf("card carries %d executable lines, want the derivation block and the restore:\n%s",
			len(out), card)
	}
	return strings.Join(out, "\n")
}
