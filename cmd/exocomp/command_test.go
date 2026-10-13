package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// TestAdhocGuardsRefuseInjection is the item-1/2 injection gate: a host pattern,
// module name, module args, or command that could split the argv or smuggle a
// flag is refused before any run starts.
func TestAdhocGuardsRefuseInjection(t *testing.T) {
	t.Parallel()

	t.Run("host pattern", func(t *testing.T) {
		for _, bad := range []string{"", "-x", "--become", "all\nfoo"} {
			if _, err := resolveHostPattern(bad, ""); err == nil {
				t.Errorf("accepted host pattern %q", bad)
			}
		}
		// --host and --limit disagree is refused; either alone is fine.
		if _, err := resolveHostPattern("web", "db"); err == nil {
			t.Error("accepted conflicting --host/--limit")
		}
		if got, err := resolveHostPattern("", "web"); err != nil || got != "web" {
			t.Errorf("--limit alias: got %q err=%v", got, err)
		}
	})

	t.Run("module name", func(t *testing.T) {
		for _, bad := range []string{"--evil", "a b", "ping;rm", ""} {
			if err := guardModule(bad); err == nil {
				t.Errorf("accepted module %q", bad)
			}
		}
		for _, ok := range []string{"command", "ansible.builtin.ping", "ansible.builtin.service"} {
			if err := guardModule(ok); err != nil {
				t.Errorf("rejected valid module %q: %v", ok, err)
			}
		}
	})

	t.Run("module args", func(t *testing.T) {
		for _, bad := range []string{"-rf /", "\t--foo", "state=up\nx"} {
			if err := guardModuleArgs(bad); err == nil {
				t.Errorf("accepted module args %q", bad)
			}
		}
		if err := guardModuleArgs("state=started name=nginx"); err != nil {
			t.Errorf("rejected valid module args: %v", err)
		}
	})

	t.Run("command tokens", func(t *testing.T) {
		if _, err := guardCommand(nil); err == nil {
			t.Error("accepted empty command")
		}
		if _, err := guardCommand([]string{"--become=root", "x"}); err == nil {
			t.Error("accepted a command starting with a dash")
		}
		if _, err := guardCommand([]string{"echo", "a\nb"}); err == nil {
			t.Error("accepted a command token with a control character")
		}
		got, err := guardCommand([]string{"systemctl", "status", "nginx"})
		if err != nil || got != "systemctl status nginx" {
			t.Errorf("guardCommand = %q err=%v", got, err)
		}
	})
}

// TestCommandRunsAdhocLocalhost is the item-1 gate: `command --host localhost --
// hostname` runs the ad-hoc command against localhost and returns its output.
func TestCommandRunsAdhocLocalhost(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible")

	var out, warn bytes.Buffer
	code, err := runAdhoc(context.Background(), &out, &warn, adhocSpec{
		runbook: "command", pattern: "localhost", module: "command", args: "echo adhoc-ran",
	})
	if err != nil {
		t.Fatalf("runAdhoc: %v\n%s%s", err, out.String(), warn.String())
	}
	if code != 0 {
		t.Fatalf("command exited %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "adhoc-ran") {
		t.Fatalf("command output missing the module result:\n%s", out.String())
	}
}

// mixedFleet writes an inventory with one reachable local host and one host that
// is deliberately unreachable (ssh to a refused port), so a run splits cleanly
// into converged and offline.
func mixedFleet(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.ini")
	writeFile(t, path, "localhost ansible_connection=local\n"+
		"laptop ansible_host=127.0.0.1 ansible_connection=ssh ansible_port=1 "+
		"ansible_user=nobody ansible_ssh_common_args='-o ConnectTimeout=2 -o BatchMode=yes'\n")
	return path
}

func getJob(t *testing.T, id int64) store.Job {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	job, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("get job %d: %v", id, err)
	}
	return job
}

// TestAdhocPartialUnreachable is the partial-success gate for the ad-hoc path: a
// mixed fleet without --tolerate-unreachable is a hard failure; with it the run
// settles degraded (succeeded, partial, exit 0) and names the offline host.
func TestAdhocPartialUnreachable(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible")
	fleet := mixedFleet(t)

	// Strict: the unreachable host fails the run.
	var out, warn bytes.Buffer
	code, err := runAdhoc(context.Background(), &out, &warn, adhocSpec{
		runbook: "command", pattern: "all", inventory: fleet, module: "ansible.builtin.ping",
	})
	if err != nil {
		t.Fatalf("runAdhoc strict: %v\n%s", err, out.String())
	}
	if code == 0 {
		t.Fatalf("strict run exited 0 with an unreachable host:\n%s", out.String())
	}
	if getJob(t, lastJobID(t, "command")).State != store.StateFailed {
		t.Fatalf("strict run did not fail:\n%s", out.String())
	}

	// Tolerant: the same fleet settles degraded, not failed.
	out.Reset()
	warn.Reset()
	code, err = runAdhoc(context.Background(), &out, &warn, adhocSpec{
		runbook: "command", pattern: "all", inventory: fleet, module: "ansible.builtin.ping",
		tolerateUnreachable: true,
	})
	if err != nil {
		t.Fatalf("runAdhoc tolerant: %v\n%s", err, out.String())
	}
	if code != 0 {
		t.Fatalf("tolerant run exited %d; a tolerated degradation must not fail:\n%s", code, out.String())
	}
	final := getJob(t, lastJobID(t, "command"))
	if final.State != store.StateDegraded || !final.Partial {
		t.Fatalf("tolerant run state=%s partial=%v, want degraded+partial", final.State, final.Partial)
	}
	events := jobEventText(t, final.ID)
	if !strings.Contains(events, "ansible_partial") || !strings.Contains(events, "laptop") {
		t.Fatalf("no partial event naming the offline host:\n%s", events)
	}
}

// TestAdhocTaskFailureStillHard proves the flag never hides a real failure: a
// reachable host whose task fails is a hard failure even when a host is also
// unreachable and --tolerate-unreachable is set.
func TestAdhocTaskFailureStillHard(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible")
	fleet := mixedFleet(t)

	var out, warn bytes.Buffer
	code, err := runAdhoc(context.Background(), &out, &warn, adhocSpec{
		runbook: "command", pattern: "all", inventory: fleet, module: "command", args: "/bin/false",
		tolerateUnreachable: true,
	})
	if err != nil {
		t.Fatalf("runAdhoc: %v\n%s", err, out.String())
	}
	if code == 0 {
		t.Fatalf("a reachable task failure exited 0 under --tolerate-unreachable:\n%s", out.String())
	}
	if final := getJob(t, lastJobID(t, "command")); final.State != store.StateFailed || final.Partial {
		t.Fatalf("state=%s partial=%v, want failed and not partial", final.State, final.Partial)
	}
}

// TestCommandAnsibleFlagEmpathy proves passing -m or -a to `exocomp command` is
// intercepted with an actionable hint pointing to `command` or `ansible`.
func TestCommandAnsibleFlagEmpathy(t *testing.T) {
	_, err := run(t, "command", "-m", "shell", "-a", "hostname")
	if err == nil {
		t.Fatal("expected error when passing -m to command, got nil")
	}
	if !strings.Contains(err.Error(), "'-m' and '-a' are Ansible flags") {
		t.Fatalf("expected empathy error message, got: %v", err)
	}
}

// TestCommandLocalBecomeRequiresPrivileged proves that requesting privilege
// escalation (--become) on a local host without --privileged is refused early.
func TestCommandLocalBecomeRequiresPrivileged(t *testing.T) {
	useDataDir(t, t.TempDir())
	var out, warn bytes.Buffer
	_, err := runAdhoc(context.Background(), &out, &warn, adhocSpec{
		runbook: "command", pattern: "localhost", module: "command", args: "whoami",
		become: true, privileged: false,
	})
	if err == nil {
		t.Fatal("expected error for local become without --privileged, got nil")
	}
	if !strings.Contains(err.Error(), "local privilege escalation with --become requires --privileged") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCommandDefaultWorkingDirHome proves that ad-hoc commands without an explicit
// --source resolve their working directory to the user's home directory.
func TestCommandDefaultWorkingDirHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no user home dir available")
	}
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible")

	var out, warn bytes.Buffer
	code, err := runAdhoc(context.Background(), &out, &warn, adhocSpec{
		runbook: "command", pattern: "localhost", module: "command", args: "pwd",
	})
	if err != nil {
		t.Fatalf("runAdhoc: %v\n%s", err, out.String())
	}
	if code != 0 {
		t.Fatalf("command exited %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), home) {
		t.Fatalf("command output = %q, want home %q", out.String(), home)
	}
}

func runCapture(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := newRootCmd()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err := root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// TestCommandOutputJSON proves `exocomp command --output json` produces structured JSON.
func TestCommandOutputJSON(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible")

	out, _, err := runCapture(t, "command", "--host", "localhost", "--output", "json", "--", "echo", "hello-json")
	if err != nil {
		t.Fatalf("command --output json failed: %v\n%s", err, out)
	}

	var env struct {
		SchemaVersion int                    `json:"schema_version"`
		Kind          string                 `json:"kind"`
		Data          wire.CommandResultView `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("failed to unmarshal JSON output: %v\noutput: %s", err, out)
	}
	if env.Kind != wire.KindCommandResult {
		t.Errorf("kind = %q, want %q", env.Kind, wire.KindCommandResult)
	}
	if env.Data.State != "succeeded" || env.Data.ExitCode != 0 {
		t.Errorf("data state/exit mismatch: %+v", env.Data)
	}
	h, ok := env.Data.Hosts["localhost"]
	if !ok {
		t.Fatalf("missing localhost in hosts: %+v", env.Data.Hosts)
	}
	if !strings.Contains(h.Stdout, "hello-json") {
		t.Errorf("localhost stdout = %q, want 'hello-json'", h.Stdout)
	}
}

// TestAnsibleAdhocOutputJSON proves `exocomp ansible --output json` also produces structured JSON.
func TestAnsibleAdhocOutputJSON(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible")

	out, _, err := runCapture(t, "ansible", "--host", "localhost", "-m", "ansible.builtin.ping", "--output", "json")
	if err != nil {
		t.Fatalf("ansible --output json failed: %v\n%s", err, out)
	}

	var env struct {
		SchemaVersion int                    `json:"schema_version"`
		Kind          string                 `json:"kind"`
		Data          wire.CommandResultView `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("failed to unmarshal JSON output: %v\noutput: %s", err, out)
	}
	if env.Kind != wire.KindCommandResult {
		t.Errorf("kind = %q, want %q", env.Kind, wire.KindCommandResult)
	}
	if env.Data.State != "succeeded" || env.Data.ExitCode != 0 {
		t.Errorf("data state/exit mismatch: %+v", env.Data)
	}
	h, ok := env.Data.Hosts["localhost"]
	if !ok {
		t.Fatalf("missing localhost in hosts: %+v", env.Data.Hosts)
	}
	if !strings.Contains(h.Stdout, "pong") {
		t.Errorf("localhost stdout = %q, want 'pong'", h.Stdout)
	}
}
