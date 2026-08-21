package secrets

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

// deriveVectors pin HKDF-SHA256 over a fixed DEK of bytes 0x00..0x1f with salt
// exocomp/v1. Cross-checked against `openssl kdf ... HKDF` before committing;
// they change only if the envelope's derivation changes, which invalidates
// every printed recovery card.
var deriveVectors = []struct {
	purpose string
	id      string
	want    string
}{
	{"restic-repo", "nas", "1303a76b24f927fa136435f4818fdc7c485764dbeafeca7f6758c8b46ab15455"},
	{"restic-repo", "s3-offsite", "22ade575b52dab916d396757ce6baf0270d8beffc44d325ce32587411e15e4f6"},
	{"tofu-state", "homelab", "709edecb9753b9c08f5e9c03877c056ca347185cc9d22c49191db1fe208fdf40"},
	{"ansible-vault", "homelab", "6d68e129df606a56970e2f2a6cad23f41d444288e79d6298e06a05415906bf2d"},
	{"webhook-hmac", "github", "1e5a69793c6780602c2d08129bff16f8f2a97c5fd03f811a8dc490ca504de226"},
	{"approval-token", "default", "381b53df148036ce0c76a4bab537cf9ea9cdffff8b37623cb6d3d78606563279"},
}

func mustInit(t *testing.T, dir string, extra []string) (*Envelope, *RecoveryKit) {
	t.Helper()
	env, kit, err := Init(dir, extra)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })
	return env, kit
}

func TestInitCreatesKeyringAndKit(t *testing.T) {
	dir := t.TempDir()
	env, kit := mustInit(t, dir, nil)

	info, err := os.Stat(filepath.Join(dir, hostKeyFile))
	if err != nil {
		t.Fatalf("stat host key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o400 {
		t.Errorf("host key mode = %o, want 400", got)
	}

	if kit.AgeSecretKey == "" {
		t.Error("kit carries no recovery secret key")
	}
	if _, err := age.ParseX25519Identity(kit.AgeSecretKey); err != nil {
		t.Errorf("kit recovery key does not parse: %v", err)
	}
	if kit.Salt != Salt {
		t.Errorf("kit salt = %q, want %q", kit.Salt, Salt)
	}
	if len(kit.Recipients) != 2 {
		t.Fatalf("kit has %d recipients, want host + recovery", len(kit.Recipients))
	}

	// The recovery secret key must exist only in the returned kit.
	for _, name := range []string{hostKeyFile, keyringFile, recipientsFile} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if bytes.Contains(b, []byte(kit.AgeSecretKey)) {
			t.Errorf("%s contains the recovery secret key", name)
		}
		if bytes.Contains(b, env.dek) {
			t.Errorf("%s contains the raw DEK", name)
		}
	}
}

func TestInitRefusesSecondRun(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir, nil)

	if _, _, err := Init(dir, nil); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Init err = %v, want ErrAlreadyInitialized", err)
	}
}

func TestInitRollsBackPartialWrite(t *testing.T) {
	dir := t.TempDir()

	// A non-empty directory where the recipients file belongs fails the write
	// and resists removal, so only the host key rollback is observable.
	blocker := filepath.Join(dir, recipientsFile)
	if err := os.MkdirAll(filepath.Join(blocker, "occupied"), 0o755); err != nil {
		t.Fatalf("stage blocker: %v", err)
	}

	if _, _, err := Init(dir, nil); err == nil {
		t.Fatal("Init succeeded with an unwritable recipients path")
	}
	if _, err := os.Stat(filepath.Join(dir, hostKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host key survived a failed Init: err = %v", err)
	}

	if err := os.RemoveAll(blocker); err != nil {
		t.Fatalf("clear blocker: %v", err)
	}
	if _, _, err := Init(dir, nil); err != nil {
		t.Fatalf("Init after rollback: %v", err)
	}
}

func TestInitExtraRecipientUnwrapsDEK(t *testing.T) {
	extra, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate extra identity: %v", err)
	}

	dir := t.TempDir()
	env, _ := mustInit(t, dir, []string{extra.Recipient().String()})

	if got := len(env.Recipients()); got != 3 {
		t.Fatalf("keyring has %d recipients, want 3", got)
	}
	keyring, err := env.KeyringAge()
	if err != nil {
		t.Fatalf("KeyringAge: %v", err)
	}
	dek, err := unwrapDEK(keyring, extra)
	if err != nil {
		t.Fatalf("unwrap with extra recipient: %v", err)
	}
	if !bytes.Equal(dek, env.dek) {
		t.Error("extra recipient unwrapped a different DEK")
	}
}

func TestInitRejectsBadRecipient(t *testing.T) {
	if _, _, err := Init(t.TempDir(), []string{"not-an-age-recipient"}); err == nil {
		t.Fatal("Init accepted a malformed recipient")
	}
}

func TestSealOpenRoundTripAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	env, _ := mustInit(t, dir, nil)

	pt := []byte("hunter2 is not a good ansible vault password")
	sealed, err := env.Seal("homelab-vault", "ansible-vault", pt)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed.Ciphertext, pt) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if len(sealed.Nonce) != nonceLen {
		t.Fatalf("nonce is %d bytes, want %d", len(sealed.Nonce), nonceLen)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	got, err := reopened.OpenSecret(sealed)
	if err != nil {
		t.Fatalf("OpenSecret: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Error("round-tripped plaintext differs")
	}
	if !bytes.Equal(reopened.dek, env.dek) {
		t.Error("Open unwrapped a different DEK")
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	env, _ := mustInit(t, t.TempDir(), nil)

	seen := make(map[string]struct{})
	for range 64 {
		s, err := env.Seal("n", "k", []byte("same plaintext every time"))
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		key := string(s.Nonce)
		if _, dup := seen[key]; dup {
			t.Fatal("nonce reused")
		}
		seen[key] = struct{}{}
	}
}

func TestOpenSecretRejectsTampering(t *testing.T) {
	env, _ := mustInit(t, t.TempDir(), nil)

	base, err := env.Seal("restic-nas", "backup-password", []byte("payload"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	tests := map[string]func(s SealedSecret) SealedSecret{
		"flipped ciphertext byte": func(s SealedSecret) SealedSecret {
			s.Ciphertext = bytes.Clone(s.Ciphertext)
			s.Ciphertext[0] ^= 0x01
			return s
		},
		"flipped tag byte": func(s SealedSecret) SealedSecret {
			s.Ciphertext = bytes.Clone(s.Ciphertext)
			s.Ciphertext[len(s.Ciphertext)-1] ^= 0x80
			return s
		},
		"flipped nonce byte": func(s SealedSecret) SealedSecret {
			s.Nonce = bytes.Clone(s.Nonce)
			s.Nonce[3] ^= 0x10
			return s
		},
		"truncated nonce": func(s SealedSecret) SealedSecret {
			s.Nonce = bytes.Clone(s.Nonce)[:nonceLen-1]
			return s
		},
		"renamed": func(s SealedSecret) SealedSecret {
			s.Name = "restic-s3"
			return s
		},
		"rekinded": func(s SealedSecret) SealedSecret {
			s.Kind = "tofu-state"
			return s
		},
		"aad boundary shifted": func(s SealedSecret) SealedSecret {
			// "restic-nas|backup-password" must not equal "restic|nas-backup-password".
			s.Name, s.Kind = "restic", "nas|backup-password"
			return s
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := env.OpenSecret(mutate(base)); err == nil {
				t.Fatal("OpenSecret accepted a tampered secret")
			}
		})
	}
}

func TestOpenRejectsWrongIdentity(t *testing.T) {
	victim, other := t.TempDir(), t.TempDir()
	mustInit(t, victim, nil)
	mustInit(t, other, nil)

	foreign, err := os.ReadFile(filepath.Join(other, keyringFile))
	if err != nil {
		t.Fatalf("read foreign keyring: %v", err)
	}
	if err := os.WriteFile(filepath.Join(victim, keyringFile), foreign, 0o644); err != nil {
		t.Fatalf("write foreign keyring: %v", err)
	}

	if _, err := Open(victim); err == nil {
		t.Fatal("Open unwrapped a keyring the host identity is not a recipient of")
	}
}

func TestOpenRejectsCorruptKeyring(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir, nil)

	path := filepath.Join(dir, keyringFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read keyring: %v", err)
	}
	b[len(b)/2] ^= 0x01
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write keyring: %v", err)
	}

	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a corrupted keyring")
	}
}

func TestDeriveVectors(t *testing.T) {
	dek := make([]byte, dekLen)
	for i := range dek {
		dek[i] = byte(i)
	}
	env := &Envelope{dek: dek}

	for _, v := range deriveVectors {
		got := hex.EncodeToString(env.Derive(v.purpose, v.id))
		if got != v.want {
			t.Errorf("Derive(%q, %q) = %s, want %s", v.purpose, v.id, got, v.want)
		}
	}
}

func TestDeriveSeparatesDomains(t *testing.T) {
	env, _ := mustInit(t, t.TempDir(), nil)

	seen := make(map[string]string)
	pairs := [][2]string{
		{"restic-repo", "nas"},
		{"restic-repo", "s3"},
		{"tofu-state", "nas"},
		// The separator must not collapse: "a:b:c" is one derivation, not two.
		{"restic-repo:nas", ""},
		{"restic-repo", ":nas"},
	}
	for _, p := range pairs {
		k := hex.EncodeToString(env.Derive(p[0], p[1]))
		if len(k) != 2*dekLen {
			t.Fatalf("Derive(%q, %q) returned %d bytes", p[0], p[1], len(k)/2)
		}
		if prev, dup := seen[k]; dup {
			t.Errorf("Derive(%q, %q) collides with %s", p[0], p[1], prev)
		}
		seen[k] = p[0] + "/" + p[1]
	}
}

func TestDeriveIsStableWithinEnvelope(t *testing.T) {
	env, _ := mustInit(t, t.TempDir(), nil)
	first := env.Derive("restic-repo", "nas")
	if !bytes.Equal(first, env.Derive("restic-repo", "nas")) {
		t.Error("Derive is not deterministic")
	}
}

func TestCloseZeroesDEK(t *testing.T) {
	env, _, err := Init(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	dek := env.dek

	if err := env.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !bytes.Equal(dek, make([]byte, dekLen)) {
		t.Error("DEK backing array not zeroed by Close")
	}
	if _, err := env.Seal("n", "k", []byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("Seal after Close: err = %v, want ErrClosed", err)
	}
	if _, err := env.OpenSecret(SealedSecret{Nonce: make([]byte, nonceLen)}); !errors.Is(err, ErrClosed) {
		t.Errorf("OpenSecret after Close: err = %v, want ErrClosed", err)
	}
	if env.Derive("restic-repo", "nas") != nil {
		t.Error("Derive after Close returned a key")
	}
	if err := env.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestKeyringAgeIsACopy(t *testing.T) {
	env, _ := mustInit(t, t.TempDir(), nil)

	first, err := env.KeyringAge()
	if err != nil {
		t.Fatalf("KeyringAge: %v", err)
	}
	first[0] ^= 0xff

	second, err := env.KeyringAge()
	if err != nil {
		t.Fatalf("KeyringAge: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("KeyringAge handed out the envelope's own buffer")
	}
	if !bytes.HasPrefix(second, []byte("-----BEGIN AGE ENCRYPTED FILE-----")) {
		t.Error("keyring is not armored")
	}
}

func TestRecipientsFileRoundTrips(t *testing.T) {
	dir := t.TempDir()
	env, _ := mustInit(t, dir, nil)

	got, err := readRecipients(filepath.Join(dir, recipientsFile))
	if err != nil {
		t.Fatalf("readRecipients: %v", err)
	}
	want := env.Recipients()
	if len(got) != len(want) {
		t.Fatalf("read %d recipients, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("recipient %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
