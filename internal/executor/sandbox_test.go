package executor_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// argvOf builds the launcher invocation for one profile, failing the test if
// the guard refuses it.
func argvOf(t *testing.T, kind string, opts executor.SandboxOptions, argv []string) []string {
	t.Helper()
	full, err := executor.ProfileFor(kind, opts).Command(argv)
	if err != nil {
		t.Fatalf("build %s command: %v", kind, err)
	}
	return full
}

// flagValues returns every value that followed name in argv.
func flagValues(argv []string, name string) []string {
	var out []string
	for i := range argv {
		if argv[i] == name && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

func TestSandboxArgvWrapsTheCommand(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	full := argvOf(t, executor.KindTofu, executor.SandboxOptions{
		Binary:       "/opt/nono",
		DataDir:      data,
		ReadPaths:    []string{filepath.Join(data, "cache", "collections")},
		WritePaths:   []string{filepath.Join(data, "run")},
		ConnectPorts: []int{41235},
	}, []string{"/usr/bin/tofu", "plan", "-no-color"})

	if got, want := full[:5], []string{"/opt/nono", "run", "--silent", "--no-audit", "--block-net"}; !slices.Equal(got, want) {
		t.Errorf("launcher prefix = %v, want %v", got, want)
	}

	sep := slices.Index(full, "--")
	if sep < 0 {
		t.Fatalf("argv has no -- separator: %v", full)
	}
	if got, want := full[sep+1:], []string{"/usr/bin/tofu", "plan", "-no-color"}; !slices.Equal(got, want) {
		t.Errorf("wrapped argv = %v, want %v", got, want)
	}

	if got, want := flagValues(full, "--read"), []string{filepath.Join(data, "cache", "collections")}; !slices.Equal(got, want) {
		t.Errorf("--read = %v, want %v", got, want)
	}
	if got, want := flagValues(full, "--allow"), []string{filepath.Join(data, "run")}; !slices.Equal(got, want) {
		t.Errorf("--allow = %v, want %v", got, want)
	}
	if got, want := flagValues(full, "--allow-connect-port"), []string{"443", "41235"}; !slices.Equal(got, want) {
		t.Errorf("--allow-connect-port = %v, want %v", got, want)
	}
}

// TestSandboxEgressIsPerKind: Landlock filters TCP by port and not by
// destination, so the port list is the whole network policy a kind gets.
func TestSandboxEgressIsPerKind(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	for _, tc := range []struct {
		kind string
		want []string
	}{
		{executor.KindAnsible, []string{"22"}},
		{executor.KindTofu, []string{"443"}},
		{"shell", nil},
		{"backup", nil},
	} {
		full := argvOf(t, tc.kind, executor.SandboxOptions{DataDir: data}, []string{"/bin/true"})
		if !slices.Contains(full, "--block-net") {
			t.Errorf("%s: profile does not block outbound network: %v", tc.kind, full)
		}
		if got := flagValues(full, "--allow-connect-port"); !slices.Equal(got, tc.want) {
			t.Errorf("%s: egress ports = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

// TestSandboxNeverGrantsAUnixSocket: ADR 0005's CVE lesson is that a sandboxed
// process reaching a powerful local unix socket is the escape. No profile this
// package builds may carry a socket grant, whatever the caller asks for.
func TestSandboxNeverGrantsAUnixSocket(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	for _, kind := range []string{executor.KindAnsible, executor.KindTofu, "shell", "probe"} {
		full := argvOf(t, kind, executor.SandboxOptions{
			DataDir:    data,
			ReadPaths:  []string{filepath.Join(data, "cache")},
			WritePaths: []string{filepath.Join(data, "run")},
		}, []string{"/bin/true"})

		for _, arg := range full {
			if strings.HasPrefix(arg, "--allow-unix-socket") {
				t.Errorf("%s: profile grants a unix socket: %v", kind, full)
			}
		}
	}
}

// TestSandboxGuardRefusesGrantsOverTheDataDir is the central guard. A grant
// covering the data dir, one of its protected files, or the filesystem root
// hands a playbook the database and the keys, which is the one thing this tier
// exists to prevent.
func TestSandboxGuardRefusesGrantsOverTheDataDir(t *testing.T) {
	t.Parallel()

	data := "/srv/exocomp"
	for _, tc := range []struct {
		name    string
		grant   string
		refused bool
	}{
		{"the data dir itself", data, true},
		{"an ancestor of the data dir", "/srv", true},
		{"the filesystem root", "/", true},
		{"the database", filepath.Join(data, "exocomp.db"), true},
		{"the write-ahead log", filepath.Join(data, "exocomp.db-wal"), true},
		{"the host key", filepath.Join(data, "host.key"), true},
		{"the keyring", filepath.Join(data, "keyring.age"), true},
		{"the daemon lock", filepath.Join(data, "serve.lock"), true},
		{"the recorded backend address", filepath.Join(data, "state-backend.addr"), true},
		{"a relative path", "run", true},
		{"the private run dir", filepath.Join(data, "run"), false},
		{"the provider cache", filepath.Join(data, "tofu", "plugin-cache"), false},
		{"the vendored collections", filepath.Join(data, "cache", "collections"), false},
		{"an unrelated tree", "/opt/tools", false},
	} {
		for _, mode := range []string{"read", "write"} {
			opts := executor.SandboxOptions{DataDir: data}
			if mode == "read" {
				opts.ReadPaths = []string{tc.grant}
			} else {
				opts.WritePaths = []string{tc.grant}
			}

			_, err := executor.ProfileFor("shell", opts).Command([]string{"/bin/true"})
			switch {
			case tc.refused && err == nil:
				t.Errorf("%s (%s): accepted, want refused", tc.name, mode)
			case !tc.refused && err != nil:
				t.Errorf("%s (%s): refused (%v), want accepted", tc.name, mode, err)
			}
		}
	}
}

// TestSandboxGuardFailsClosedWithoutADataDir: a profile that does not know
// what to protect cannot protect it, and building one anyway would be a
// sandbox that looks enforced and is not.
func TestSandboxGuardFailsClosedWithoutADataDir(t *testing.T) {
	t.Parallel()

	if _, err := executor.ProfileFor("shell", executor.SandboxOptions{}).Command([]string{"/bin/true"}); err == nil {
		t.Fatal("a profile with no data dir was accepted")
	}
}

// TestSandboxGrantsTheWorkingDirectory: nono refuses a working directory it
// holds no grant on, and a run's own directory is one it always needs.
func TestSandboxGrantsTheWorkingDirectory(t *testing.T) {
	t.Parallel()

	data, work := t.TempDir(), t.TempDir()
	var got []string
	inner := driverFunc(func(_ context.Context, spec executor.Spec, _ executor.EventSink) (executor.Result, error) {
		got = spec.Argv
		return executor.Result{}, nil
	})

	sb := executor.NewSandbox("shell", executor.SandboxOptions{DataDir: data}, inner)
	if _, err := sb.Run(context.Background(),
		executor.Spec{Argv: []string{"/bin/true"}, Dir: work}, executor.SinkFunc(func(executor.Event) {}),
	); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !slices.Contains(flagValues(got, "--allow"), work) {
		t.Errorf("working directory %s is not granted: %v", work, got)
	}
}

// TestSandboxRefusesAWorkingDirectoryOverTheDataDir: the working directory is a
// grant like any other, so it goes through the same guard.
func TestSandboxRefusesAWorkingDirectoryOverTheDataDir(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	sb := executor.NewSandbox("shell", executor.SandboxOptions{DataDir: data}, nil)
	_, err := sb.Run(context.Background(),
		executor.Spec{Argv: []string{"/bin/true"}, Dir: data}, executor.SinkFunc(func(executor.Event) {}))
	if err == nil {
		t.Fatal("a run whose working directory is the data dir was accepted")
	}
}

// driverFunc adapts a function to executor.Driver.
type driverFunc func(context.Context, executor.Spec, executor.EventSink) (executor.Result, error)

// Run implements executor.Driver.
func (f driverFunc) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (
	executor.Result, error) {
	return f(ctx, spec, sink)
}
