package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runRunner executes `exocomp runner ...` through the whole root command, so
// the persistent --output flag resolves exactly as it does for an operator.
func runRunner(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return run(t, append([]string{"runner"}, args...)...)
}

const testBase = "registry.fedoraproject.org/fedora-minimal:44"

func TestRunnerAddListShowRm(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	bootstrap := filepath.Join(dir, "bootstrap.sh")
	if err := os.WriteFile(bootstrap, []byte("microdnf install -y sshpass\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runRunner(t, "add", "fedora-ee", "--base", testBase,
		"--bootstrap-file", bootstrap, "--packages", "sshpass,rsync",
		"--ansible-core", "2.18.1", "--memory-max", "2G", "--cpu-quota", "200%")
	if err != nil {
		t.Fatalf("runner add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "registered cached runner \"fedora-ee\"") {
		t.Errorf("add output = %q", out)
	}
	if !strings.Contains(out, deferredNote) {
		t.Errorf("add output does not state the deferred status: %q", out)
	}

	listed, err := runRunner(t, "list")
	if err != nil {
		t.Fatalf("runner list: %v (%s)", err, listed)
	}
	if !strings.Contains(listed, "fedora-ee") || !strings.Contains(listed, "cached") {
		t.Errorf("list output = %q", listed)
	}

	shown, err := runRunner(t, "show", "fedora-ee")
	if err != nil {
		t.Fatalf("runner show: %v (%s)", err, shown)
	}
	for _, want := range []string{"rsync sshpass", "2.18.1", "2G", "200%",
		"microdnf install -y sshpass", deferredNote} {
		if !strings.Contains(shown, want) {
			t.Errorf("show output lacks %q:\n%s", want, shown)
		}
	}

	// The hash must be the same string in the list, the show, and the add.
	hash := runnerHashFromJSON(t, "fedora-ee")
	for name, text := range map[string]string{"add": out, "show": shown} {
		if !strings.Contains(text, hash) {
			t.Errorf("%s output does not carry hash %s:\n%s", name, hash, text)
		}
	}
	if !strings.Contains(listed, hash[:12]) {
		t.Errorf("list output does not carry the hash prefix %s:\n%s", hash[:12], listed)
	}

	removed, err := runRunner(t, "rm", "fedora-ee")
	if err != nil {
		t.Fatalf("runner rm: %v (%s)", err, removed)
	}
	if after, err := runRunner(t, "list"); err != nil || !strings.Contains(after, "no runners") {
		t.Errorf("list after rm = %q err=%v", after, err)
	}
	if _, err := runRunner(t, "rm", "fedora-ee"); err == nil {
		t.Error("removing an unknown runner succeeded")
	}
}

// runnerHashFromJSON reads one runner's content hash back through the wire
// envelope, which also proves `--output json` emits the envelope shape.
func runnerHashFromJSON(t *testing.T, name string) string {
	t.Helper()
	out, err := runRunner(t, "show", name, "--output", "json")
	if err != nil {
		t.Fatalf("runner show --output json: %v (%s)", err, out)
	}
	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Kind          string `json:"kind"`
		Data          struct {
			Name            string   `json:"name"`
			Mode            string   `json:"mode"`
			ContentHash     string   `json:"content_hash"`
			Packages        []string `json:"packages"`
			BootstrapSHA256 string   `json:"bootstrap_sha256"`
			BootstrapLines  int      `json:"bootstrap_lines"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("show json is not valid JSON: %v\n%s", err, out)
	}
	if env.SchemaVersion != 1 || env.Kind != "runner.get" {
		t.Fatalf("unexpected envelope: version=%d kind=%q", env.SchemaVersion, env.Kind)
	}
	if env.Data.Name != name || env.Data.ContentHash == "" {
		t.Fatalf("unexpected payload: %+v", env.Data)
	}
	if strings.Contains(out, "microdnf") {
		t.Errorf("json show carries the bootstrap body: %s", out)
	}
	return env.Data.ContentHash
}

func TestRunnerListJSONEnvelope(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	empty, err := runRunner(t, "list", "--output", "json")
	if err != nil {
		t.Fatalf("empty list json: %v (%s)", err, empty)
	}
	if empty != `{"schema_version":1,"kind":"runner.list","data":[]}`+"\n" {
		t.Errorf("empty list envelope = %q", empty)
	}

	if out, err := runRunner(t, "add", "web", "--base", testBase); err != nil {
		t.Fatalf("runner add: %v (%s)", err, out)
	}
	out, err := runRunner(t, "list", "--output", "json")
	if err != nil {
		t.Fatalf("list json: %v (%s)", err, out)
	}
	var env struct {
		Kind string `json:"kind"`
		Data []struct {
			Name        string `json:"name"`
			Mode        string `json:"mode"`
			ContentHash string `json:"content_hash"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("list json is not valid JSON: %v\n%s", err, out)
	}
	if env.Kind != "runner.list" || len(env.Data) != 1 || env.Data[0].Name != "web" {
		t.Fatalf("unexpected list payload: %s", out)
	}
	if env.Data[0].Mode != "cached" {
		t.Errorf("default mode = %q, want cached", env.Data[0].Mode)
	}
}

func TestRunnerAddValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	bootstrap := filepath.Join(dir, "bootstrap.sh")
	if err := os.WriteFile(bootstrap, []byte("true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no base", []string{"add", "ee"}, "needs a base image"},
		{"bad name", []string{"add", "Fedora EE", "--base", testBase}, "must start with"},
		{"bad mode", []string{"add", "ee", "--base", testBase, "--mode", "podman"}, "mode must be"},
		{"both models", []string{"add", "ee", "--base", testBase,
			"--bootstrap-file", bootstrap, "--containerfile", ".exocomp/Containerfile"},
			"at most one of --bootstrap-file or --containerfile"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := runRunner(t, c.args...)
			if err == nil {
				t.Fatalf("accepted %v: %s", c.args, out)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}

	if out, err := runRunner(t, "add", "dupe", "--base", testBase); err != nil {
		t.Fatalf("first add: %v (%s)", err, out)
	}
	_, err := runRunner(t, "add", "dupe", "--base", testBase)
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("duplicate add error = %v, want a duplicate-key error", err)
	}
}

// TestRunnerContainerfileSelectsOnTheFly pins the design §2 default: naming a
// Containerfile picks on-the-fly semantics unless --mode says otherwise.
func TestRunnerContainerfileSelectsOnTheFly(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	if out, err := runRunner(t, "add", "dev", "--base", testBase,
		"--containerfile", ".exocomp/Containerfile"); err != nil {
		t.Fatalf("add: %v (%s)", err, out)
	}
	out, err := runRunner(t, "show", "dev")
	if err != nil {
		t.Fatalf("show: %v (%s)", err, out)
	}
	if !strings.Contains(out, "mode:          onthefly") {
		t.Errorf("show output = %q, want mode onthefly", out)
	}

	if out, err := runRunner(t, "add", "pinned", "--base", testBase,
		"--containerfile", ".exocomp/Containerfile", "--mode", "cached"); err != nil {
		t.Fatalf("add with explicit mode: %v (%s)", err, out)
	}
	shown, err := runRunner(t, "show", "pinned")
	if err != nil {
		t.Fatalf("show: %v (%s)", err, shown)
	}
	if !strings.Contains(shown, "mode:          cached") {
		t.Errorf("explicit --mode lost: %q", shown)
	}
}

const wrappedSpec = `runner:
  mode: cached
  base: registry.fedoraproject.org/fedora-minimal:44
  bootstrap: |
    microdnf install -y sshpass rsync
  packages: [sshpass, rsync]
  ansible_core: "2.18.1"
  build:
    no_cache: false
  caps:
    memory_max: 2G
    cpu_quota: "200%"
`

const bareSpec = `name: tofu-ee
base: registry.fedoraproject.org/fedora-minimal:44
containerfile: .exocomp/Containerfile
`

func TestRunnerImport(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	specs := filepath.Join(root, "runners")
	if err := os.MkdirAll(specs, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(specs, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("fedora-ee.yaml", wrappedSpec)
	write("tofu.yaml", bareSpec)

	out, err := runRunner(t, "import", specs)
	if err != nil {
		t.Fatalf("runner import: %v (%s)", err, out)
	}
	// The wrapped spec takes its name from the file; the bare one names itself.
	if !strings.Contains(out, "created fedora-ee") || !strings.Contains(out, "created tofu-ee") {
		t.Fatalf("import output = %q", out)
	}

	listed, err := runRunner(t, "list")
	if err != nil {
		t.Fatalf("list: %v (%s)", err, listed)
	}
	if !strings.Contains(listed, "fedora-ee") || !strings.Contains(listed, "tofu-ee") {
		t.Errorf("list after import = %q", listed)
	}
	shown, err := runRunner(t, "show", "fedora-ee")
	if err != nil {
		t.Fatalf("show: %v (%s)", err, shown)
	}
	for _, want := range []string{"microdnf install -y sshpass rsync", "2G", "200%", "rsync sshpass"} {
		if !strings.Contains(shown, want) {
			t.Errorf("imported runner lacks %q:\n%s", want, shown)
		}
	}

	// Re-importing an unchanged spec updates in place and keeps the hash.
	first := runnerHashFromJSON(t, "fedora-ee")
	again, err := runRunner(t, "import", specs)
	if err != nil {
		t.Fatalf("second import: %v (%s)", err, again)
	}
	if !strings.Contains(again, "updated fedora-ee") {
		t.Errorf("second import output = %q, want an update", again)
	}
	if second := runnerHashFromJSON(t, "fedora-ee"); second != first {
		t.Errorf("re-import moved the hash: %s -> %s", first, second)
	}

	// An edited spec moves the hash.
	write("fedora-ee.yaml", strings.Replace(wrappedSpec, "sshpass rsync", "sshpass rsync git", 1))
	if out, err := runRunner(t, "import", specs); err != nil {
		t.Fatalf("third import: %v (%s)", err, out)
	}
	if edited := runnerHashFromJSON(t, "fedora-ee"); edited == first {
		t.Errorf("an edited bootstrap left the hash at %s", first)
	}
}

func TestRunnerImportRejectsBadSpec(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	specs := filepath.Join(root, "runners")
	if err := os.MkdirAll(specs, 0o700); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"typo":       "base: img\nbootstrp: true\n",
		"no base":    "name: ee\nmode: cached\n",
		"two models": "name: ee\nbase: img\nbootstrap: true\ncontainerfile: Containerfile\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(specs, "spec.yaml")
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := runRunner(t, "import", specs); err == nil {
				t.Fatalf("imported an invalid spec: %s", out)
			}
		})
	}

	if err := os.Remove(filepath.Join(specs, "spec.yaml")); err != nil {
		t.Fatal(err)
	}
	out, err := runRunner(t, "import", specs)
	if err != nil {
		t.Fatalf("import of an empty dir: %v (%s)", err, out)
	}
	if !strings.Contains(out, "no runner specs") {
		t.Errorf("empty dir output = %q", out)
	}
}
