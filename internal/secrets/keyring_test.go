package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitWritesTheFirstKeyringVersion(t *testing.T) {
	dir := t.TempDir()
	env, kit := mustInit(t, dir, nil)

	got, err := ReadKeyringVersion(dir)
	if err != nil {
		t.Fatalf("ReadKeyringVersion: %v", err)
	}
	if got.Version != 1 {
		t.Errorf("fresh keyring is %s, want v1", got)
	}
	if got.Generated.IsZero() {
		t.Error("fresh keyring version records no generation time")
	}
	if env.KeyringVersion() != got {
		t.Errorf("envelope reports %v, file says %v", env.KeyringVersion(), got)
	}
	if !strings.Contains(kit.Render(), "kit v1") {
		t.Error("the minted card does not name its kit version")
	}
	if strings.Contains(kit.Render(), "replaces") {
		t.Error("the first card claims to replace an earlier one")
	}
}

// TestKeyringVersionParsesBothForms pins the compatibility rule: a data dir
// written before versioning existed carries no version file at all, and that
// absence is generation 1 rather than an error.
func TestKeyringVersionParsesBothForms(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir, nil)

	path := filepath.Join(dir, KeyringVersionName)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove version file: %v", err)
	}
	legacy, err := ReadKeyringVersion(dir)
	if err != nil {
		t.Fatalf("ReadKeyringVersion on a legacy dir: %v", err)
	}
	if legacy.Version != 1 {
		t.Errorf("legacy keyring reads as %s, want v1", legacy)
	}
	if !legacy.Generated.IsZero() {
		t.Error("legacy keyring invented a generation time")
	}
	env, err := Open(dir)
	if err != nil {
		t.Fatalf("Open a legacy dir: %v", err)
	}
	defer func() { _ = env.Close() }()
	if env.KeyringVersion().Version != 1 {
		t.Errorf("legacy envelope reports %s, want v1", env.KeyringVersion())
	}

	stamp := time.Date(2026, 8, 22, 9, 30, 0, 0, time.UTC)
	if err := os.WriteFile(path, MarshalKeyringVersion(KeyringVersion{Version: 7, Generated: stamp}), 0o644); err != nil {
		t.Fatalf("write version file: %v", err)
	}
	versioned, err := ReadKeyringVersion(dir)
	if err != nil {
		t.Fatalf("ReadKeyringVersion: %v", err)
	}
	if versioned.Version != 7 || !versioned.Generated.Equal(stamp) {
		t.Errorf("versioned keyring reads as %v, want v7 at %s", versioned, stamp)
	}
}

func TestKeyringVersionRejectsUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyringVersionName)

	for name, body := range map[string]string{
		"no version key":  "generated: 2026-08-22T09:30:00Z\n",
		"unparsable":      "version: two\n",
		"bad timestamp":   "version: 2\ngenerated: yesterday\n",
		"zeroth version":  "version: 0\n",
		"negative number": "version: -3\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatalf("write version file: %v", err)
			}
			if v, err := ReadKeyringVersion(dir); err == nil {
				t.Errorf("ReadKeyringVersion accepted %q as %v", body, v)
			}
		})
	}
}

func TestKitVersionLineNamesTheReplacedGeneration(t *testing.T) {
	kit := &RecoveryKit{Version: KeyringVersion{
		Version:   3,
		Generated: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
	}}

	line := kit.VersionLine()
	for _, want := range []string{"kit v3", "generated 2026-08-22", "replaces v2 (destroy it)"} {
		if !strings.Contains(line, want) {
			t.Errorf("version line %q omits %q", line, want)
		}
	}
}
