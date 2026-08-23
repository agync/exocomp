package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
)

// TestSealStatusTextUnchanged pins the unconfigured-state text, bare vs
// --output text.
func TestSealStatusTextUnchanged(t *testing.T) {
	useDataDir(t, t.TempDir())

	bare, err := run(t, "seal", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bare, "absent") || !strings.Contains(bare, "not configured") {
		t.Fatalf("bare = %q", bare)
	}
	explicit, err := run(t, "seal", "status", "--output", "text")
	if err != nil {
		t.Fatal(err)
	}
	if explicit != bare {
		t.Fatalf("--output text (%q) differs from default (%q)", explicit, bare)
	}
}

// TestSealStatusJSONUnconfigured proves the envelope shape when neither the
// blob nor the env is present.
func TestSealStatusJSONUnconfigured(t *testing.T) {
	useDataDir(t, t.TempDir())

	out, err := run(t, "seal", "status", "--output", "json")
	if err != nil {
		t.Fatalf("seal status --output json: %v (%s)", err, out)
	}
	var env sealEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if env.SchemaVersion != 1 || env.Kind != "seal.status" {
		t.Fatalf("bad envelope header: %+v", env)
	}
	if env.Data.TransitFilePresent || env.Data.EnvConfigured || env.Data.OrphanBlob {
		t.Fatalf("unexpected facts for an unconfigured seal: %+v", env.Data)
	}
}

// TestSealStatusJSONConfiguredNeverLeaksToken seeds a fully configured seal
// env with a sentinel token and asserts the token never reaches the JSON,
// while the address and key name (identifiers, not secrets) still do.
func TestSealStatusJSONConfiguredNeverLeaksToken(t *testing.T) {
	useDataDir(t, t.TempDir())
	t.Setenv(secrets.EnvSealAddress, "https://bao.lan:8200")
	t.Setenv(secrets.EnvSealKey, "exocomp")
	t.Setenv(secrets.EnvSealToken, "TOKEN-LEAK-MARKER")

	text, err := run(t, "seal", "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "TOKEN-LEAK-MARKER") {
		t.Fatalf("text status leaked the token: %q", text)
	}

	out, err := run(t, "seal", "status", "--output", "json")
	if err != nil {
		t.Fatalf("seal status --output json: %v (%s)", err, out)
	}
	if strings.Contains(out, "TOKEN-LEAK-MARKER") {
		t.Fatalf("seal status json leaked the token: %s", out)
	}
	var env sealEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if !env.Data.EnvConfigured || env.Data.SealAddress != "https://bao.lan:8200" || env.Data.SealKeyName != "exocomp" {
		t.Fatalf("json facts do not match the configured seal: %+v", env.Data)
	}
}

// TestSealStatusJSONOrphanBlob: a transit blob on disk with no seal env set is
// the "would fall back to the host key" note in both text and json.
func TestSealStatusJSONOrphanBlob(t *testing.T) {
	dir := t.TempDir()
	useDataDir(t, dir)
	dataPath := filepath.Join(dir, "exocomp")
	if err := os.MkdirAll(dataPath, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataPath, secrets.TransitSealName), []byte("vault:v1:orphan"), 0o600); err != nil {
		t.Fatalf("seed transit blob: %v", err)
	}

	text, err := run(t, "seal", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "present") || !strings.Contains(text, "note:") {
		t.Fatalf("text did not report the orphan blob: %q", text)
	}

	out, err := run(t, "seal", "status", "--output", "json")
	if err != nil {
		t.Fatalf("seal status --output json: %v (%s)", err, out)
	}
	var env sealEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if !env.Data.TransitFilePresent || env.Data.EnvConfigured || !env.Data.OrphanBlob {
		t.Fatalf("json facts do not match the orphan blob: %+v", env.Data)
	}
}

type sealEnvelope struct {
	SchemaVersion int `json:"schema_version"`
	Kind          string
	Data          struct {
		TransitFilePresent bool   `json:"transit_file_present"`
		EnvConfigured      bool   `json:"env_configured"`
		EnvError           string `json:"env_error"`
		SealAddress        string `json:"seal_address"`
		SealKeyName        string `json:"seal_key_name"`
		OrphanBlob         bool   `json:"orphan_blob"`
	}
}
