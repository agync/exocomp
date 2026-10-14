package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/metrics"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// doctorEnvelope is the shape doctor --output json must produce.
type doctorEnvelope struct {
	SchemaVersion int `json:"schema_version"`
	Kind          string
	Data          []struct {
		Name   string
		Status string
		Detail string
	}
}

// wantSubsystems is doctor's fixed, ordered health projection — the six
// subsystems in scope for the JSON summary (docs/design/agent-interface.md
// §3.2). A drift here is a schema change.
var wantSubsystems = []string{
	"data_dir", "secrets_protection", "optional_tools", "self_observability", "litestream", "privilege_posture",
}

func validHealthStatus(s string) bool {
	switch s {
	case "ok", "warn", "fail", "unknown":
		return true
	default:
		return false
	}
}

// TestDoctorJSONEnvelope proves the envelope shape and that every in-scope
// subsystem is reported, in the fixed order, with a status from the small
// enum — one pass/fail per subsystem in a single call.
func TestDoctorJSONEnvelope(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	if err := os.MkdirAll(filepath.Join(root, "exocomp"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	out, err := run(t, "doctor", "--output", "json")
	if err != nil {
		t.Fatalf("doctor --output json: %v (%s)", err, out)
	}

	var env doctorEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if env.SchemaVersion != 1 || env.Kind != "doctor" {
		t.Fatalf("bad envelope header: %+v", env)
	}
	if len(env.Data) != len(wantSubsystems) {
		t.Fatalf("subsystem count = %d, want %d: %s", len(env.Data), len(wantSubsystems), out)
	}
	for i, name := range wantSubsystems {
		entry := env.Data[i]
		if entry.Name != name {
			t.Errorf("entry %d name = %q, want %q (order must be stable): %s", i, entry.Name, name, out)
		}
		if !validHealthStatus(entry.Status) {
			t.Errorf("entry %q status = %q, not one of ok|warn|fail|unknown", entry.Name, entry.Status)
		}
	}
}

// TestDoctorJSONViaEnv proves EXOCOMP_OUTPUT=json takes the same path as
// --output json.
func TestDoctorJSONViaEnv(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	if err := os.MkdirAll(filepath.Join(root, "exocomp"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	t.Setenv("EXOCOMP_OUTPUT", "json")

	out, err := run(t, "doctor")
	if err != nil {
		t.Fatalf("EXOCOMP_OUTPUT=json doctor: %v (%s)", err, out)
	}
	var env doctorEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("EXOCOMP_OUTPUT=json did not produce an envelope: %q", out)
	}
	if env.Kind != "doctor" {
		t.Fatalf("kind = %q, want doctor", env.Kind)
	}
}

// TestDoctorJSONCoversTheFaultPath: a data dir doctor's text mode would abort
// on still yields the full summary — the fault becomes one subsystem's status,
// not a missing report. This is the not-fully-healthy path the golden in
// internal/wire pins the bytes for.
func TestDoctorJSONCoversTheFaultPath(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	dir := filepath.Join(root, "exocomp")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	out, err := run(t, "doctor", "--output", "json")
	if err != nil {
		t.Fatalf("doctor --output json on a loose data dir: %v (%s)", err, out)
	}
	var env doctorEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if len(env.Data) == 0 || env.Data[0].Name != "data_dir" || env.Data[0].Status != "fail" {
		t.Fatalf("data_dir entry should be fail: %s", out)
	}
	if !strings.Contains(env.Data[0].Detail, "TOO PERMISSIVE") {
		t.Errorf("data_dir detail does not name the fault: %s", out)
	}
	// The rest of the summary must still be present — that is the point of
	// not short-circuiting on the JSON path.
	if len(env.Data) != len(wantSubsystems) {
		t.Fatalf("fault path dropped subsystems: got %d, want %d: %s", len(env.Data), len(wantSubsystems), out)
	}
}

// TestDoctorJSONNoScrapeTokenOrSecret seeds a real host key and envelope —
// the actual material probeMetricsResult would present as a bearer — and
// proves neither the token, the key, nor sealed-column markers ever reach the
// JSON summary.
func TestDoctorJSONNoScrapeTokenOrSecret(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)

	if _, err := run(t, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	dir := filepath.Join(root, "exocomp")
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	token := metrics.ScrapeToken(env)
	_ = env.Close()
	if token == "" {
		t.Fatal("expected a non-empty scrape token to check against")
	}

	out, err := run(t, "doctor", "--output", "json")
	if err != nil {
		t.Fatalf("doctor --output json: %v (%s)", err, out)
	}
	for _, banned := range []string{token, "Bearer", "nonce", "ciphertext", "aad"} {
		if strings.Contains(out, banned) {
			t.Fatalf("doctor json leaked %q: %s", banned, out)
		}
	}
}

// TestDoctorReportsTheImageRef: the deployment image ref the recovery chain
// names is visible on both doctor faces, and an absent ref is stated plainly.
func TestDoctorReportsTheImageRef(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	if _, err := run(t, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.PutSetting(t.Context(), store.SettingImageRef, "ghcr.io/abn/exocomp:test"); err != nil {
		t.Fatalf("put image ref: %v", err)
	}
	_ = st.Close()

	var out bytes.Buffer
	if err := runDoctor(&out); err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "image ref          ghcr.io/abn/exocomp:test") {
		t.Errorf("text report does not name the image ref:\n%s", out.String())
	}

	js, err := run(t, "doctor", "--output", "json")
	if err != nil {
		t.Fatalf("doctor --output json: %v (%s)", err, js)
	}
	if !strings.Contains(js, "image-ref: ghcr.io/abn/exocomp:test") {
		t.Errorf("optional_tools detail does not carry the image ref: %s", js)
	}
}

// TestDoctorSaysWhenNoImageRefRecorded: an unset ref is said, not blanked.
func TestDoctorSaysWhenNoImageRefRecorded(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	if _, err := run(t, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	var out bytes.Buffer
	if err := runDoctor(&out); err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "image ref          not recorded — set EXOCOMP_IMAGE on the unit") {
		t.Errorf("text report does not say the ref is unrecorded:\n%s", out.String())
	}
}

// TestDoctorDefaultTextEqualsExplicit mirrors TestDefaultTextUnchanged for
// doctor specifically: the bare invocation must equal --output text, and
// neither may look like JSON.
func TestDoctorDefaultTextEqualsExplicit(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	if err := os.MkdirAll(filepath.Join(root, "exocomp"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}

	bare, err := run(t, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v (%s)", err, bare)
	}
	explicit, err := run(t, "doctor", "--output", "text")
	if err != nil {
		t.Fatalf("doctor --output text: %v (%s)", err, explicit)
	}
	if bare != explicit {
		t.Errorf("doctor: --output text differs from default:\n bare: %q\nexplicit: %q", bare, explicit)
	}
	if strings.HasPrefix(strings.TrimSpace(bare), "{") {
		t.Errorf("doctor default output must not be JSON: %q", bare)
	}
}

// TestDoctorTextRefactorPreservedBytes pins the parts of the text report this
// task touched (secrets protection, optional tools, self-observability,
// litestream) byte-for-byte, with PATH emptied so every optional tool and the
// litestream binary are deterministically absent regardless of host. The
// sandbox block is intentionally not pinned here — it is not one of this
// task's subsystems and its content (Landlock ABI) is kernel-dependent.
func TestDoctorTextRefactorPreservedBytes(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	if err := os.MkdirAll(filepath.Join(root, "exocomp"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	t.Setenv("PATH", "")
	t.Setenv(health.ManifestEnv, "")

	var out bytes.Buffer
	if err := runDoctor(&out); err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, out.String())
	}
	got := out.String()

	if want := "go runtime:   " + runtime.Version() + "\n"; !strings.HasPrefix(got, want) {
		t.Errorf("go runtime line changed:\n got prefix: %q\nwant prefix: %q", got[:min(len(got), len(want))], want)
	}
	wantDataDir := "data dir:     " + filepath.Join(root, "exocomp") + " — mode 0700 — writable\n"
	if !strings.Contains(got, wantDataDir) {
		t.Errorf("data dir line changed:\n%s\nwant it to contain:\n%s", got, wantDataDir)
	}

	wantOptionalTools := "optional tools (probe subset):\n" +
		"  ansible-playbook   MISSING\n" +
		"  tofu               MISSING\n" +
		"  restic             MISSING\n" +
		"  age                MISSING\n" +
		"  sops               MISSING\n" +
		"  uv                 MISSING\n" +
		"  git                MISSING\n"
	if !strings.Contains(got, wantOptionalTools) {
		t.Errorf("optional tools block changed:\n%s\nwant it to contain:\n%s", got, wantOptionalTools)
	}

	if !strings.Contains(got, "  host key at rest   absent — run `exocomp init`\n") {
		t.Errorf("secrets protection host-key line changed:\n%s", got)
	}
	if !strings.Contains(got, "  metrics            daemon not running (no address file)\n") ||
		!strings.Contains(got, "  dead-man           not configured — no external heartbeat wired yet\n") ||
		!strings.Contains(got, "  silence/mute       no database yet\n") {
		t.Errorf("self-observability block changed:\n%s", got)
	}
	wantLitestream := "litestream sidecar:\n" +
		"  binary             not installed (optional) — install litestream (https://litestream.io/install) and re-run\n" +
		"  target             no database yet\n"
	if !strings.Contains(got, wantLitestream) {
		t.Errorf("litestream block changed:\n%s\nwant it to contain:\n%s", got, wantLitestream)
	}
}
