package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// unwrapWith opens the keyring in dir with one identity, which is how a
// recipient that is neither the host nor the operator's card proves it is (or
// is no longer) on the keyring.
func unwrapWith(t *testing.T, dir string, id age.Identity) ([]byte, error) {
	t.Helper()
	keyring, err := os.ReadFile(filepath.Join(dir, keyringFile))
	if err != nil {
		t.Fatalf("read keyring: %v", err)
	}
	return unwrapDEK(keyring, id)
}

func TestMigrateRoundTripsARecipient(t *testing.T) {
	dir := t.TempDir()
	env, kit := mustInit(t, dir, nil)

	sealed, err := env.Seal("canary", "generic", []byte("still-openable"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	dek := bytes.Clone(env.dek)
	restic := bytes.Clone(env.Derive("restic-repo", "nas"))
	if err := env.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	laptop, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate laptop identity: %v", err)
	}

	added, err := Migrate(dir, MigrateOptions{Add: []string{"laptop=" + laptop.Recipient().String()}})
	if err != nil {
		t.Fatalf("Migrate add: %v", err)
	}
	if added.From.Version != 1 || added.To.Version != 2 {
		t.Errorf("add moved %s to %s, want v1 to v2", added.From, added.To)
	}
	if !strings.Contains(added.Nag(), "destroy the v1 page") {
		t.Errorf("migration nag does not name the stale page: %s", added.Nag())
	}
	if got, err := unwrapWith(t, dir, laptop); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("the added recipient cannot unwrap the keyring: %v", err)
	}

	// The DEK is untouched by a re-wrap, so every sealed secret and every
	// derived subkey survives it — the property ADR 0003 buys with the envelope.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after add: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.KeyringVersion().Version != 2 {
		t.Errorf("reopened envelope is %s, want v2", reopened.KeyringVersion())
	}
	pt, err := reopened.OpenSecret(sealed)
	if err != nil {
		t.Fatalf("OpenSecret after add: %v", err)
	}
	if string(pt) != "still-openable" {
		t.Errorf("secret opened as %q after a re-wrap", pt)
	}
	if !bytes.Equal(reopened.Derive("restic-repo", "nas"), restic) {
		t.Error("re-wrapping the keyring changed a derived subkey")
	}
	if !strings.Contains(reopened.KitStructure().Render(), "kit v2 · generated") {
		t.Error("the re-rendered card does not carry the new generation")
	}

	dropped, err := Migrate(dir, MigrateOptions{Drop: []string{"laptop"}})
	if err != nil {
		t.Fatalf("Migrate drop: %v", err)
	}
	if dropped.To.Version != 3 {
		t.Errorf("drop moved to %s, want v3", dropped.To)
	}
	if len(dropped.Dropped) != 1 || dropped.Dropped[0].Label != "laptop" {
		t.Errorf("drop reports %v", dropped.Dropped)
	}
	if _, err := unwrapWith(t, dir, laptop); err == nil {
		t.Fatal("the dropped recipient still unwraps the keyring")
	}

	// The printed card is the one thing a migration must never lock out.
	recovery, err := age.ParseX25519Identity(kit.AgeSecretKey)
	if err != nil {
		t.Fatalf("parse recovery key: %v", err)
	}
	if got, err := unwrapWith(t, dir, recovery); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("the printed recovery key no longer unwraps the keyring: %v", err)
	}
}

func TestMigrateRotatesTheHostKey(t *testing.T) {
	dir := t.TempDir()
	env, _ := mustInit(t, dir, nil)
	dek := bytes.Clone(env.dek)

	oldRaw, err := os.ReadFile(filepath.Join(dir, hostKeyFile))
	if err != nil {
		t.Fatalf("read host key: %v", err)
	}
	oldHost, err := age.ParseX25519Identity(strings.TrimSpace(string(oldRaw)))
	if err != nil {
		t.Fatalf("parse host key: %v", err)
	}
	if err := env.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res, err := Migrate(dir, MigrateOptions{RotateHost: true})
	if err != nil {
		t.Fatalf("Migrate rotate: %v", err)
	}
	if !res.HostRotated || res.To.Version != 2 {
		t.Errorf("rotate reported %+v", res)
	}

	rotated, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after rotation: %v", err)
	}
	defer func() { _ = rotated.Close() }()
	if !bytes.Equal(rotated.dek, dek) {
		t.Error("rotating the host key changed the DEK")
	}
	if _, err := unwrapWith(t, dir, oldHost); err == nil {
		t.Fatal("the replaced host identity still unwraps the keyring")
	}

	info, err := os.Stat(filepath.Join(dir, hostKeyFile))
	if err != nil {
		t.Fatalf("stat rotated host key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o400 {
		t.Errorf("rotated host key mode = %o, want 400", got)
	}
	if _, err := os.Stat(filepath.Join(dir, hostKeyFile+".new")); !os.IsNotExist(err) {
		t.Error("rotation left its temporary host key behind")
	}
}

func TestMigrateRefusals(t *testing.T) {
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}

	for name, opts := range map[string]MigrateOptions{
		"no change":        {},
		"drop the host":    {Drop: []string{hostLabel}},
		"drop the card":    {Drop: []string{recoveryLabel}},
		"drop an unknown":  {Drop: []string{"nas-yubikey"}},
		"add a duplicate":  {Add: []string{"twin=" + other.Recipient().String(), "twin2=" + other.Recipient().String()}},
		"add a non-key":    {Add: []string{"laptop=not-an-age-key"}},
		"add an own again": {Add: []string{"again=" + other.Recipient().String()}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			env, _ := mustInit(t, dir, []string{other.Recipient().String()})
			before := env.KeyringVersion()
			if err := env.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			if _, err := Migrate(dir, opts); err == nil {
				t.Fatal("Migrate accepted the change")
			}
			after, err := ReadKeyringVersion(dir)
			if err != nil {
				t.Fatalf("ReadKeyringVersion: %v", err)
			}
			if after.Version != before.Version {
				t.Errorf("a refused migration bumped the keyring to %s", after)
			}
		})
	}
}
