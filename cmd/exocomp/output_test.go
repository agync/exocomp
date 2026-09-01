package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// seedStore points the CLI's data-dir resolution at a temp dir and returns an
// open store on it. The command under test resolves the same dir via
// XDG_DATA_HOME, so what the test seeds is what the command reads.
func seedStore(t *testing.T) *store.Store {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	dir := filepath.Join(tmp, "exocomp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// run executes the whole root command with args and returns combined output.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// TestDefaultTextUnchanged pins the byte-for-byte empty-state text of a couple
// representative verbs, and proves the bare invocation equals --output text.
func TestDefaultTextUnchanged(t *testing.T) {
	seedStore(t)

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"workspace", "list"}, "no workspaces\n"},
		{[]string{"secrets", "list"}, "no sealed secrets\n"},
		{[]string{"jobs", "list"}, "no jobs\n"},
	}
	for _, c := range cases {
		bare, err := run(t, c.args...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if bare != c.want {
			t.Errorf("%v default text = %q, want %q", c.args, bare, c.want)
		}
		explicit, err := run(t, append(append([]string{}, c.args...), "--output", "text")...)
		if err != nil {
			t.Fatalf("%v --output text: %v", c.args, err)
		}
		if explicit != bare {
			t.Errorf("%v: --output text (%q) differs from default (%q)", c.args, explicit, bare)
		}
		if strings.HasPrefix(strings.TrimSpace(bare), "{") {
			t.Errorf("%v default output must not be JSON: %q", c.args, bare)
		}
	}
}

// TestJSONEnvelopeAndEnv covers --output json, EXOCOMP_OUTPUT, and that the flag
// overrides the env.
func TestJSONEnvelopeAndEnv(t *testing.T) {
	seedStore(t)

	type env struct {
		SchemaVersion int             `json:"schema_version"`
		Kind          string          `json:"kind"`
		Data          json.RawMessage `json:"data"`
	}

	out, err := run(t, "workspace", "list", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var e env
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if e.SchemaVersion != 1 || e.Kind != "workspace.list" || string(e.Data) != "[]" {
		t.Fatalf("bad envelope: %+v", e)
	}

	t.Setenv("EXOCOMP_OUTPUT", "json")
	out, err = run(t, "secrets", "list")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("EXOCOMP_OUTPUT=json did not produce an envelope: %q", out)
	}
	if e.Kind != "secret.list" {
		t.Fatalf("kind = %q, want secret.list", e.Kind)
	}

	// Flag overrides env.
	out, err = run(t, "secrets", "list", "--output", "text")
	if err != nil {
		t.Fatal(err)
	}
	if out != "no sealed secrets\n" {
		t.Fatalf("--output text must override EXOCOMP_OUTPUT=json, got %q", out)
	}
}

// TestSecretsListJSONNeverLeaksValue seeds a secret whose sealed columns hold
// unmistakable markers and asserts none reach the JSON.
func TestSecretsListJSONNeverLeaksValue(t *testing.T) {
	st := seedStore(t)
	if _, err := st.CreateSecret(context.Background(), store.SecretRow{
		Name:       "vault/ansible",
		Kind:       "password",
		Nonce:      []byte("NONCE-LEAK-MARKER"),
		Ciphertext: []byte("CIPHERTEXT-LEAK-MARKER"),
		AAD:        "AAD-LEAK-MARKER",
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	out, err := run(t, "secrets", "list", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"NONCE-LEAK-MARKER", "CIPHERTEXT-LEAK-MARKER", "AAD-LEAK-MARKER", "nonce", "ciphertext"} {
		if strings.Contains(out, banned) {
			t.Fatalf("secrets list --output json leaked %q: %s", banned, out)
		}
	}
	if !strings.Contains(out, "vault/ansible") {
		t.Fatalf("secrets list should still name the secret: %s", out)
	}
}

// TestJobsListShowsRawRunbook: the CLI list prints the raw stored runbook in
// the RUNBOOK column — the exact value --runbook matches (rule: prefix
// included), so a runbook read off the list is pasteable back into a filter —
// while jobs get keeps both the stripped display name and a raw line, and the
// JSON view (a machine surface) keeps the raw value.
func TestJobsListShowsRawRunbook(t *testing.T) {
	st := seedStore(t)
	job, err := st.CreateJob(context.Background(), store.NewJob{
		Runbook: "rule:deploy-app", Kind: "tofu.plan", Trigger: "rule", ConcurrencyKey: "rule:deploy-app",
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}

	text, err := run(t, "jobs", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "rule:deploy-app") {
		t.Fatalf("jobs list text missing the raw runbook value: %q", text)
	}

	get, err := run(t, "jobs", "get", strconv.FormatInt(job.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	// The detail view keeps the stripped rule name for humans...
	if !strings.Contains(get, "deploy-app") {
		t.Fatalf("jobs get text missing the stripped runbook name: %q", get)
	}
	// ...and prints the raw stored runbook too, so an operator who needs the
	// exact key --runbook filters on can read it off the detail page.
	if !strings.Contains(get, "runbook (raw)") || !strings.Contains(get, "rule:deploy-app") {
		t.Fatalf("jobs get text missing the raw runbook line: %q", get)
	}

	jsonOut, err := run(t, "jobs", "list", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Kind string `json:"kind"`
		Data []struct {
			Runbook string `json:"runbook"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &e); err != nil {
		t.Fatalf("jobs list json: %v (%q)", err, jsonOut)
	}
	if e.Kind != "job.list" || len(e.Data) != 1 || e.Data[0].Runbook != "rule:deploy-app" {
		t.Fatalf("jobs list json did not preserve the raw runbook: %s", jsonOut)
	}
}

func TestJobsListTextAndJSONSeeded(t *testing.T) {
	st := seedStore(t)
	if _, err := st.CreateJob(context.Background(), store.NewJob{
		Runbook: "web", Kind: "tofu.plan", Trigger: "manual", ConcurrencyKey: "workspace:web",
		Args: store.JobArgs{Source: "my-fleet"},
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	text, err := run(t, "jobs", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "ID") || !strings.Contains(text, "STATE") || !strings.Contains(text, "queued") {
		t.Fatalf("jobs list text missing header/row: %q", text)
	}
	if !strings.Contains(text, "SOURCE") || !strings.Contains(text, "my-fleet") {
		t.Fatalf("jobs list text missing SOURCE column/value: %q", text)
	}

	jsonOut, err := run(t, "jobs", "list", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Kind string `json:"kind"`
		Data []struct {
			Runbook string `json:"runbook"`
			Source  string `json:"source"`
			State   string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &e); err != nil {
		t.Fatalf("jobs list json: %v (%q)", err, jsonOut)
	}
	if e.Kind != "job.list" || len(e.Data) != 1 || e.Data[0].Runbook != "web" || e.Data[0].State != "queued" {
		t.Fatalf("unexpected job list json: %s", jsonOut)
	}
	if e.Data[0].Source != "my-fleet" {
		t.Fatalf("job list json source = %q, want %q", e.Data[0].Source, "my-fleet")
	}
}
