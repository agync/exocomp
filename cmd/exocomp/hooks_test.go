package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// seedPathSource registers a path source rooted at root, so `hooks show`
// resolves it without a daemon, a checkout, or network access.
func seedPathSource(t *testing.T, st *store.Store, name, root string) {
	t.Helper()
	if _, err := st.CreateSource(context.Background(), store.Source{
		Name: name, Kind: store.SourcePath, URI: root, Enabled: true,
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
}

// TestHooksShowTextUnchanged pins that --output text equals the bare
// invocation for a source with a hooks.yml.
func TestHooksShowTextUnchanged(t *testing.T) {
	st := seedStore(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".exocomp"), 0o755); err != nil {
		t.Fatalf("mkdir .exocomp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".exocomp", "hooks.yml"),
		[]byte("pre_run:\n  - name: prep\n    run: \"true\"\n"+
			"post_run:\n  - name: notify\n    script: scripts/notify.sh\n    secret: vault/slack\n"+
			"gate:\n  - name: health\n    http:\n      url: http://127.0.0.1:9/\n"),
		0o644); err != nil {
		t.Fatalf("write hooks.yml: %v", err)
	}
	seedPathSource(t, st, "web", root)

	bare, err := run(t, "hooks", "show", "web")
	if err != nil {
		t.Fatalf("hooks show: %v (%s)", err, bare)
	}
	if !strings.Contains(bare, "pre_run:") || !strings.Contains(bare, "prep") ||
		!strings.Contains(bare, "post_run:") || !strings.Contains(bare, "notify") ||
		!strings.Contains(bare, "gate:") || !strings.Contains(bare, "health") {
		t.Fatalf("bare missing expected facts: %q", bare)
	}
	explicit, err := run(t, "hooks", "show", "web", "--output", "text")
	if err != nil {
		t.Fatal(err)
	}
	if explicit != bare {
		t.Fatalf("--output text (%q) differs from default (%q)", explicit, bare)
	}
}

// TestHooksShowJSON proves the envelope shape and that the JSON facts match
// the text facts, including the secret NAME (never a value).
func TestHooksShowJSON(t *testing.T) {
	st := seedStore(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".exocomp"), 0o755); err != nil {
		t.Fatalf("mkdir .exocomp: %v", err)
	}
	hooksPath := filepath.Join(root, ".exocomp", "hooks.yml")
	if err := os.WriteFile(hooksPath,
		[]byte("pre_run:\n  - name: prep\n    run: \"true\"\n"+
			"post_run:\n  - name: notify\n    script: scripts/notify.sh\n    secret: vault/slack\n"+
			"gate:\n  - name: health\n    http:\n      url: http://127.0.0.1:9/\n"),
		0o644); err != nil {
		t.Fatalf("write hooks.yml: %v", err)
	}
	seedPathSource(t, st, "web", root)

	out, err := run(t, "hooks", "show", "web", "--output", "json")
	if err != nil {
		t.Fatalf("hooks show --output json: %v (%s)", err, out)
	}
	var env struct {
		SchemaVersion int `json:"schema_version"`
		Kind          string
		Data          struct {
			Source    string `json:"source"`
			HooksPath string `json:"hooks_path"`
			Override  bool   `json:"override"`
			PreRun    []struct {
				Name string `json:"name"`
				Run  string `json:"run"`
			} `json:"pre_run"`
			PostRun []struct {
				Name   string `json:"name"`
				Script string `json:"script"`
				Secret string `json:"secret"`
			} `json:"post_run"`
			Gate []struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"gate"`
		}
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if env.SchemaVersion != 1 || env.Kind != "hooks.show" {
		t.Fatalf("bad envelope header: %+v", env)
	}
	if env.Data.Source != "web" || env.Data.HooksPath != hooksPath || env.Data.Override {
		t.Fatalf("json header facts wrong: %+v", env.Data)
	}
	if len(env.Data.PreRun) != 1 || env.Data.PreRun[0].Name != "prep" || env.Data.PreRun[0].Run != "true" {
		t.Fatalf("pre_run facts wrong: %+v", env.Data.PreRun)
	}
	if len(env.Data.PostRun) != 1 || env.Data.PostRun[0].Name != "notify" ||
		env.Data.PostRun[0].Script != "scripts/notify.sh" || env.Data.PostRun[0].Secret != "vault/slack" {
		t.Fatalf("post_run facts wrong: %+v", env.Data.PostRun)
	}
	if len(env.Data.Gate) != 1 || env.Data.Gate[0].Name != "health" || env.Data.Gate[0].Kind != "http" {
		t.Fatalf("gate facts wrong: %+v", env.Data.Gate)
	}
	if strings.Contains(out, "127.0.0.1") {
		t.Fatalf("hooks show json leaked the gate probe's target: %s", out)
	}
}

// TestHooksShowJSONNoHooksFile proves a source with no hooks.yml still yields
// a well-formed envelope with empty (never null) phase lists.
func TestHooksShowJSONNoHooksFile(t *testing.T) {
	st := seedStore(t)
	seedPathSource(t, st, "bare", t.TempDir())

	out, err := run(t, "hooks", "show", "bare", "--output", "json")
	if err != nil {
		t.Fatalf("hooks show --output json: %v (%s)", err, out)
	}
	if strings.TrimSpace(out) != `{"schema_version":1,"kind":"hooks.show","data":{"source":"bare","pre_run":[],"post_run":[],"gate":[]}}` {
		t.Fatalf("no-hooks json = %q", out)
	}
}

// TestHooksShowJSONNotFoundExitCode proves the JSON path exits
// wire.ExitNotFound for an unknown source, matching the text path.
func TestHooksShowJSONNotFoundExitCode(t *testing.T) {
	seedStore(t)

	_, err := run(t, "hooks", "show", "no-such-source", "--output", "json")
	if err == nil {
		t.Fatal("want an error for an unknown source")
	}
	if exitCodeFor(err) != 3 {
		t.Fatalf("exit code = %d, want 3 (not_found)", exitCodeFor(err))
	}

	_, textErr := run(t, "hooks", "show", "no-such-source")
	if textErr == nil {
		t.Fatal("want an error for an unknown source (text path)")
	}
	if exitCodeFor(textErr) != 3 {
		t.Fatalf("text exit code = %d, want 3 (not_found)", exitCodeFor(textErr))
	}
}
