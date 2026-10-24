package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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

// selections are the four ways a remote gets selected; the hosts.toml default
// needs no input at all.
var selections = map[string]func(*testing.T, []string) []string{
	"flag": func(_ *testing.T, args []string) []string { return append([]string{"--remote", "box"}, args...) },
	"env": func(t *testing.T, args []string) []string {
		t.Setenv(remoteEnv, "box")
		return args
	},
	"empty flag": func(_ *testing.T, args []string) []string { return append([]string{"--remote="}, args...) },
	"default":    func(_ *testing.T, args []string) []string { return args },
}

func TestRemoteSelectedRefusesCommandsWithoutARemotePath(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"source", "add", "demo", "--url", "https://example.invalid/demo.git"}, "no remote path yet"},
		{[]string{"source", "rm", "demo"}, "no remote path yet"},
		{[]string{"run-secret", "add", "demo", "TOKEN", "tok"}, "no remote path yet"},
		{[]string{"secrets", "add", "tok"}, "no remote path yet"},
		{[]string{"schedule", "rm", "nightly"}, "no remote path yet"},
		{[]string{"rule", "rm", "r"}, "no remote path yet"},
		{[]string{"jobs", "redrive"}, "runs only on the daemon's host"},
		{[]string{"token", "new", "ci"}, "runs only on the daemon's host"},
		{[]string{"identity", "migrate"}, "runs only on the daemon's host"},
		{[]string{"approval", "link", "1"}, "runs only on the daemon's host"},
		// The keyring readers read this machine's envelope and store.
		{[]string{"kit", "derive", "--list"}, "runs only on the daemon's host"},
		{[]string{"kit", "verify"}, "runs only on the daemon's host"},
		{[]string{"recovery-kit"}, "runs only on the daemon's host"},
		{[]string{"seal", "status"}, "runs only on the daemon's host"},
		{[]string{"litestream", "status"}, "runs only on the daemon's host"},
	}
	for _, tc := range cases {
		for name, withRemote := range selections {
			t.Run(strings.Join(tc.args[:min(2, len(tc.args))], " ")+"/"+name, func(t *testing.T) {
				pinnedRemote(t, http.NotFoundHandler())
				t.Setenv(remoteEnv, "")
				dir := isolatedDataDir(t)
				_, _, err := runRoot(t, withRemote(t, tc.args))
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--local") {
					t.Fatalf("want a refusal saying %q and naming --local, got %v", tc.want, err)
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
	t.Setenv(remoteEnv, "")
	isolatedDataDir(t)

	for _, args := range [][]string{{"version"}, {"commands"}, {"help"}, {"completion", "bash"}} {
		if _, _, err := runRoot(t, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	// A remote-capable verb reaches the daemon; the stub answers 404, which is an
	// API error, not the guard's refusal.
	_, _, err := runRoot(t, []string{"source", "list"})
	if err == nil || strings.Contains(err.Error(), "remote path") {
		t.Fatalf("source list should reach the remote and fail on its answer, got %v", err)
	}
}

// With a default remote stored, a bare command goes to it; --local keeps it on
// this machine.
func TestBareCommandUsesTheDefaultRemote(t *testing.T) {
	pinnedRemote(t, jobsAPIStub(t))
	t.Setenv(remoteEnv, "")
	dir := isolatedDataDir(t)

	out, _, err := runRoot(t, []string{"jobs", "list"})
	if err != nil {
		t.Fatalf("bare jobs list: %v", err)
	}
	if !strings.Contains(out, "web") {
		t.Fatalf("bare jobs list did not render the remote's jobs:\n%s", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a remote command touched the local data dir: %v", err)
	}

	if _, _, err := runRoot(t, []string{"--local", "source", "list"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("--local should use the local store: %v", err)
	}
}

func TestLocalRunsAPendingCommandHere(t *testing.T) {
	pinnedRemote(t, http.NotFoundHandler())
	t.Setenv(remoteEnv, "")
	dir := isolatedDataDir(t)

	if _, _, err := runRoot(t, []string{"--local", "source", "add", "nas", "--path", "/srv/nas"}); err != nil {
		t.Fatalf("--local source add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "exocomp.db")); err != nil {
		t.Fatalf("--local should write the local store: %v", err)
	}
}

func TestLocalAndRemoteTogetherAreRefused(t *testing.T) {
	pinnedRemote(t, http.NotFoundHandler())
	dir := isolatedDataDir(t)

	_, _, err := runRoot(t, []string{"--local", "--remote", "box", "source", "list"})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want a --local/--remote conflict, got %v", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("data dir was touched: %v", statErr)
	}
}

// --local=false is not --local: with --remote it is no conflict, and the
// command goes to the remote.
func TestLocalFalseWithRemoteGoesRemote(t *testing.T) {
	pinnedRemote(t, jobsAPIStub(t))
	dir := isolatedDataDir(t)

	out, _, err := runRoot(t, []string{"--local=false", "--remote", "box", "jobs", "list", "--output", "json"})
	if err != nil {
		t.Fatalf("--local=false --remote box: %v", err)
	}
	if !strings.Contains(out, `"runbook":"web"`) {
		t.Fatalf("output %q is not the remote's job list", out)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("data dir was touched: %v", statErr)
	}
}

// With no hosts.toml at all, a bare command is local as before.
func TestNoConfiguredRemoteStaysLocal(t *testing.T) {
	withConfigHome(t)
	t.Setenv(remoteEnv, "")
	dir := isolatedDataDir(t)

	if _, _, err := runRoot(t, []string{"source", "list"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a bare command with no remote should use the local store: %v", err)
	}
}

// A hosts.toml that cannot be read may hold a default, so a command neither
// guesses local nor remote.
func TestUnreadableHostsConfigFailsClosed(t *testing.T) {
	path := withConfigHome(t)
	t.Setenv(remoteEnv, "")
	dir := isolatedDataDir(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("default = [broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := runRoot(t, []string{"source", "list"})
	if err == nil || !strings.Contains(err.Error(), "--local") {
		t.Fatalf("want a hosts.toml error naming --local, got %v", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("data dir was touched: %v", statErr)
	}
	if _, _, err := runRoot(t, []string{"--local", "source", "list"}); err != nil {
		t.Fatalf("--local should not need hosts.toml: %v", err)
	}
}

// runnablePaths walks the tree for every command a user can run, skipping
// cobra's hidden completion helpers.
func runnablePaths(root *cobra.Command) []string {
	var paths []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c != root && c.Runnable() && !strings.HasPrefix(c.Name(), "__") {
			path, _ := commandKey(c)
			paths = append(paths, path)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return paths
}

// fullTree builds the root with the help and completion commands cobra only adds
// at Execute, so the walk sees what a user can run.
func fullTree() *cobra.Command {
	root := newRootCmd()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	return root
}

// The drift test: every runnable command is remote-capable, exempt, local-only
// on purpose, or pending its API route, and in exactly one of those sets. A new
// command fails here until it is classified.
func TestEveryCommandIsClassified(t *testing.T) {
	for _, path := range runnablePaths(fullTree()) {
		_, local := localOnly[path]
		in := 0
		for _, ok := range []bool{remoteCapable[path], remoteExempt[path], local, remotePending[path]} {
			if ok {
				in++
			}
		}
		if in != 1 {
			t.Errorf("%q is in %d of remoteCapable, remoteExempt, localOnly and remotePending; "+
				"want exactly 1 (Decision 0026)", path, in)
		}
	}
}

// Every classified path must name a real runnable command, so a rename cannot
// leave a set silently stale and a parent's name cannot stand in for its
// subcommands.
func TestClassifiedPathsAreCommands(t *testing.T) {
	root := fullTree()
	var keys []string
	for k := range remoteCapable {
		keys = append(keys, k)
	}
	for k := range remoteExempt {
		keys = append(keys, k)
	}
	for k := range localOnly {
		keys = append(keys, k)
	}
	for k := range remotePending {
		keys = append(keys, k)
	}
	for _, path := range keys {
		c, _, err := root.Find(strings.Fields(path))
		if err != nil || c == nil || c.CommandPath() != "exocomp "+path || !c.Runnable() {
			t.Errorf("%q is classified but is not a runnable command", path)
		}
	}
}

// Both refusals, pending and local-only, are a wire error envelope whose code
// is remote_unsupported, so an agent can branch on it.
func TestRemoteRefusalIsAWireErrorUnderJSON(t *testing.T) {
	pinnedRemote(t, http.NotFoundHandler())
	isolatedDataDir(t)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"source", "rm", "demo"}, "no remote path yet"},
		{[]string{"token", "list"}, "runs only on the daemon's host"},
	} {
		out, _, err := runRoot(t, append([]string{"--remote", "box", "--output", "json"}, tc.args...))
		if err == nil || !strings.Contains(out, tc.want) {
			t.Fatalf("%v: want an error envelope on stdout, got err=%v out=%q", tc.args, err, out)
		}
		var env struct {
			Kind  string     `json:"kind"`
			Error wire.Error `json:"error"`
		}
		if jerr := json.Unmarshal([]byte(out), &env); jerr != nil {
			t.Fatalf("%v: refusal is not one JSON envelope: %v\n%s", tc.args, jerr, out)
		}
		if env.Kind != wire.KindError || env.Error.Code != wire.CodeRemoteUnsupported || env.Error.Hint == "" {
			t.Fatalf("%v: envelope = %+v, want code %s and a hint", tc.args, env, wire.CodeRemoteUnsupported)
		}
		if code := exitCodeFor(err); code != wire.ExitFailure {
			t.Fatalf("%v: exit code = %d, want %d", tc.args, code, wire.ExitFailure)
		}
	}
}

// Each token verb's refusal gives its own reason: list reads, it mints nothing.
func TestTokenRefusalsNameWhatEachVerbDoes(t *testing.T) {
	pinnedRemote(t, http.NotFoundHandler())
	isolatedDataDir(t)

	for _, tc := range []struct {
		args       []string
		want, deny string
	}{
		{[]string{"token", "list"}, "reads the host's token store", "minted"},
		{[]string{"token", "new", "ci"}, "minted on the host", "revoked"},
		{[]string{"token", "rotate", "ci"}, "minted on the host", "revoked"},
		{[]string{"token", "revoke", "ci"}, "revoked on the host", "minted"},
	} {
		_, _, err := runRoot(t, append([]string{"--remote", "box"}, tc.args...))
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.deny) {
			t.Fatalf("%v: refusal = %v, want %q and not %q", tc.args, err, tc.want, tc.deny)
		}
	}
}

// The root guard only runs if no subcommand brings its own persistent pre-run
// hook (cobra runs the nearest one) and none shadows --local or --remote with a
// flag of its own.
func TestNoCommandBypassesTheRootGuard(t *testing.T) {
	root := fullTree()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c != root && (c.PersistentPreRun != nil || c.PersistentPreRunE != nil) {
			t.Errorf("%q has its own persistent pre-run hook, which replaces the root guard", c.CommandPath())
		}
		for _, name := range []string{localFlag, remoteFlag} {
			if c != root && c.LocalFlags().Lookup(name) != nil {
				t.Errorf("%q declares its own --%s, which shadows the root flag", c.CommandPath(), name)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// --local wins over EXOCOMP_REMOTE as it does over a stored default.
func TestLocalBeatsTheEnvironment(t *testing.T) {
	pinnedRemote(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a --local command must not contact the remote")
	}))
	t.Setenv(remoteEnv, "box")
	dir := isolatedDataDir(t)

	if _, _, err := runRoot(t, []string{"--local", "source", "list"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("--local should use the local store: %v", err)
	}
}

// A default naming an alias with no pinned entry is an error that names
// --local, never a quiet local run.
func TestDefaultNamingAnUnpinnedAliasFailsClosed(t *testing.T) {
	withConfigHome(t)
	t.Setenv(remoteEnv, "")
	dir := isolatedDataDir(t)
	if err := saveHostsConfig(hostsConfig{Default: "gone", Hosts: map[string]hostEntry{}}); err != nil {
		t.Fatal(err)
	}

	_, _, err := runRoot(t, []string{"source", "list"})
	if err == nil || !strings.Contains(err.Error(), "not pinned") || !strings.Contains(err.Error(), "--local") {
		t.Fatalf("want a not-pinned error naming --local, got %v", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("data dir was touched: %v", statErr)
	}
}

// The recovery path runs on this machine whatever hosts.toml names, so
// packaging/recovery.sh keeps working on a host with a default remote.
func TestRestoreIgnoresTheDefaultRemote(t *testing.T) {
	pinnedRemote(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("restore must not contact the remote")
	}))
	t.Setenv(remoteEnv, "")
	isolatedDataDir(t)

	_, _, err := runRoot(t, []string{"restore"})
	if err == nil || strings.Contains(err.Error(), "--local") || !strings.Contains(err.Error(), "--from") {
		t.Fatalf("want restore's own --from refusal, not the guard's, got %v", err)
	}
}
