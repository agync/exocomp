package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

// isolatedDataDir points the data dir at a temp dir and returns it, so a test can
// assert that a refused command left nothing behind.
func isolatedDataDir(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	return filepath.Join(xdg, "exocomp")
}

func TestRemoteSelectedRefusesLocalOnlyCommands(t *testing.T) {
	pinnedRemote(t, http.NotFoundHandler())
	dir := isolatedDataDir(t)

	cases := [][]string{
		{"source", "add", "demo", "--url", "https://example.invalid/demo.git"},
		{"source", "rm", "demo"},
		{"source", "sync", "demo"},
		{"source", "egress", "demo", "443"},
		{"run-secret", "add", "demo", "TOKEN", "tok"},
		{"inventory", "add", "inv"},
		{"secrets", "add", "tok"},
		{"schedule", "rm", "nightly"},
		{"rule", "rm", "r"},
		{"token", "new", "ci"},
		{"host", "onboard", "box"},
		{"backup", "run"},
		{"runner", "rm", "r"},
		{"policy", "rm", "p"},
		{"silence", "rm", "1"},
		{"jobs", "redrive"},
		{"hooks", "show", "demo"},
	}
	for _, args := range cases {
		for name, withRemote := range map[string]func(*testing.T) []string{
			"flag": func(*testing.T) []string { return append([]string{"--remote", "box"}, args...) },
			"env": func(t *testing.T) []string {
				t.Setenv(remoteEnv, "box")
				return args
			},
			"empty flag": func(*testing.T) []string { return append([]string{"--remote="}, args...) },
		} {
			t.Run(strings.Join(args[:2], " ")+"/"+name, func(t *testing.T) {
				_, _, err := runRoot(t, withRemote(t))
				if err == nil || !strings.Contains(err.Error(), "no remote support") {
					t.Fatalf("want a no remote support error, got %v", err)
				}
				if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
					t.Fatalf("data dir was touched: %v", statErr)
				}
			})
		}
	}
}

func TestRemoteSelectedKeepsRemoteCapableAndExemptCommands(t *testing.T) {
	pinnedRemote(t, http.NotFoundHandler())
	isolatedDataDir(t)

	for _, args := range [][]string{{"version"}, {"commands"}, {"help"}, {"completion", "bash"}} {
		if _, _, err := runRoot(t, append([]string{"--remote", "box"}, args...)); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	// A remote-capable verb reaches the daemon; the stub answers 404, which is an
	// API error, not the guard's refusal.
	_, _, err := runRoot(t, []string{"--remote", "box", "source", "list"})
	if err == nil || strings.Contains(err.Error(), "no remote support") {
		t.Fatalf("source list should reach the remote and fail on its answer, got %v", err)
	}
}

func TestBareCommandIgnoresStoredDefaultRemote(t *testing.T) {
	pinnedRemote(t, http.NotFoundHandler())
	t.Setenv(remoteEnv, "")
	dir := isolatedDataDir(t)

	if _, _, err := runRoot(t, []string{"source", "list"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a bare command should use the local store: %v", err)
	}
}

// Every path in remoteCapable must name a real command, so a rename cannot leave
// the allowlist silently stale.
func TestRemoteCapableNamesExistingCommands(t *testing.T) {
	root := newRootCmd()
	for path := range remoteCapable {
		c, _, err := root.Find(strings.Fields(path))
		if err != nil || c == nil || c.CommandPath() != "exocomp "+path {
			t.Errorf("remoteCapable %q is not a command", path)
		}
	}
}

func TestRemoteRefusalIsAWireErrorUnderJSON(t *testing.T) {
	pinnedRemote(t, http.NotFoundHandler())
	isolatedDataDir(t)

	out, _, err := runRoot(t, []string{"--remote", "box", "--output", "json", "source", "rm", "demo"})
	if err == nil || !strings.Contains(out, "no remote support") {
		t.Fatalf("want an error envelope on stdout, got err=%v out=%q", err, out)
	}
	var env struct {
		Kind  string     `json:"kind"`
		Error wire.Error `json:"error"`
	}
	if jerr := json.Unmarshal([]byte(out), &env); jerr != nil {
		t.Fatalf("refusal is not one JSON envelope: %v\n%s", jerr, out)
	}
	if env.Kind != wire.KindError || env.Error.Code != wire.CodeRemoteUnsupported || env.Error.Hint == "" {
		t.Fatalf("envelope = %+v, want kind error with code %s and a hint", env, wire.CodeRemoteUnsupported)
	}
	if code := exitCodeFor(err); code != wire.ExitFailure {
		t.Fatalf("exit code = %d, want %d", code, wire.ExitFailure)
	}
}
