package executor_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

// fakeAnsible writes a stand-in ansible-playbook and returns its path.
func fakeAnsible(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), executor.AnsibleBinary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake ansible-playbook: %v", err)
	}
	return path
}

func runAnsible(t *testing.T, opts executor.AnsibleOptions, argv []string) (executor.Result, *collector) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var sink collector
	res, err := executor.NewAnsible(opts).Run(ctx,
		executor.Spec{Kind: executor.KindAnsible, Argv: argv}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res, &sink
}

func TestAnsibleArgv(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		inventory string
		want      []string
	}{
		{
			name: "empty defers to ansible discovery",
			want: []string{"ansible-playbook", "/p/site.yml"},
		},
		{
			name:      "inventory",
			inventory: "/p/hosts.ini",
			want:      []string{"ansible-playbook", "--inventory", "/p/hosts.ini", "/p/site.yml"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := executor.AnsibleArgv("/p/site.yml", tc.inventory)
			if !slices.Equal(got, tc.want) {
				t.Errorf("argv = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAnsibleBuildsInvocation pins what the child actually receives: the
// vault file in the private directory, and an environment built from an
// allowlist rather than inherited wholesale.
func TestAnsibleBuildsInvocation(t *testing.T) {
	t.Setenv("EXOCOMP_CANARY", "must-not-reach-the-child")
	t.Setenv("ANSIBLE_STDOUT_CALLBACK", "minimal")

	bin := fakeAnsible(t, `
for a in "$@"; do
	echo "arg:$a"
	case "$a" in
		*@*) f="${a#*@}"; [ -f "$f" ] && echo "vaultfile:$(cat "$f")" ;;
	esac
done
env | sed 's/^/env:/'
`)
	root := t.TempDir()

	res, sink := runAnsible(t, executor.AnsibleOptions{
		Binary:         bin,
		VaultPassword:  []byte("pw-from-the-envelope"),
		ExtraVars:      map[string]string{"project": "homelab"},
		PrivateDirRoot: root,
	}, executor.AnsibleArgv("/p/site.yml", ""))

	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	lines := sink.msgs(executor.LevelInfo)
	has := func(prefix, want string) bool {
		return slices.Contains(lines, prefix+want)
	}

	for _, want := range []string{"/p/site.yml", "--vault-id", "--extra-vars"} {
		if !has("arg:", want) {
			t.Errorf("argument %q missing from %q", want, lines)
		}
	}
	// An empty inventory adds no --inventory: the run defers to ansible's own
	// inventory discovery rather than pinning localhost.
	for _, absent := range []string{"--inventory", "localhost,", "--connection"} {
		if has("arg:", absent) {
			t.Errorf("argument %q should be absent for an empty inventory: %q", absent, lines)
		}
	}
	if !has("vaultfile:", "pw-from-the-envelope") {
		t.Errorf("vault password file not readable by the child: %q", lines)
	}

	var vaultArg, extraArg string
	for _, l := range lines {
		switch arg := strings.TrimPrefix(l, "arg:"); {
		case strings.HasPrefix(arg, executor.DefaultVaultID+"@"):
			vaultArg = arg
		case strings.HasPrefix(arg, "@"):
			extraArg = arg
		}
	}
	if vaultArg == "" {
		t.Fatalf("no %s@file argument in %q", executor.DefaultVaultID, lines)
	}
	if extraArg == "" {
		t.Fatalf("no @file extra-vars argument in %q", lines)
	}
	if dir := filepath.Dir(strings.TrimPrefix(vaultArg, executor.DefaultVaultID+"@")); filepath.Dir(dir) != root {
		t.Errorf("vault file lives in %s, want a directory under %s", dir, root)
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "env:EXOCOMP_CANARY=") {
			t.Errorf("the parent environment leaked into the child: %q", line)
		}
	}
	for _, want := range []string{"env:PATH=", "env:ANSIBLE_STDOUT_CALLBACK=minimal"} {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, want) }) {
			t.Errorf("%q missing from the child environment: %q", want, lines)
		}
	}

	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read private dir root: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("private directory survived the run: %v", left)
	}
}

// TestAnsiblePythonPathBridge is the Decision 0016 gate on the run environment: the
// venv's site-packages is set on PYTHONPATH so the PATH-resolved fallback
// ansible imports the project's libraries. PYTHONPATH is not on the inherited
// allowlist, so an operator's own value is dropped rather than merged; without
// a bridge the run carries no PYTHONPATH at all.
func TestAnsiblePythonPathBridge(t *testing.T) {
	const bridge = "/data/cache/python/demo/abc123/lib/python3.14/site-packages"
	bin := fakeAnsible(t, `env | sed 's/^/env:/'`)

	t.Run("bridge set, inherited PYTHONPATH dropped", func(t *testing.T) {
		t.Setenv("PYTHONPATH", "/operator/lib")
		_, sink := runAnsible(t, executor.AnsibleOptions{
			Binary:         bin,
			PythonPath:     bridge,
			PrivateDirRoot: t.TempDir(),
		}, executor.AnsibleArgv("/p/site.yml", ""))

		want := "env:PYTHONPATH=" + bridge
		if lines := sink.msgs(executor.LevelInfo); !slices.Contains(lines, want) {
			t.Errorf("child environment missing %q: %q", want, lines)
		}
	})

	t.Run("no bridge means no PYTHONPATH", func(t *testing.T) {
		t.Setenv("PYTHONPATH", "/operator/lib")
		_, sink := runAnsible(t, executor.AnsibleOptions{
			Binary:         bin,
			PrivateDirRoot: t.TempDir(),
		}, executor.AnsibleArgv("/p/site.yml", ""))

		for _, l := range sink.msgs(executor.LevelInfo) {
			if strings.HasPrefix(l, "env:PYTHONPATH=") {
				t.Errorf("run with no bridge carried %q", l)
			}
		}
	})
}

// dumpExtraVars writes a fake ansible-playbook that prints the JSON body of the
// --extra-vars @file the driver hands it, so a test can assert exactly which
// variables a run forces on every host.
func dumpExtraVars(t *testing.T) string {
	t.Helper()
	return fakeAnsible(t, `
prev=""
for a in "$@"; do
	if [ "$prev" = "--extra-vars" ]; then
		echo "extravars:$(cat "${a#@}")"
	fi
	prev="$a"
done
`)
}

// TestAnsibleInterpreterScopedToLocalConnection is the regression guard for the
// remote rc-127 defect: the source venv interpreter is the controller's own and
// must never be forced as a global ansible_python_interpreter that a remote host
// would inherit — the remote has no such path. It stays forced only for an
// explicit `localhost,` inline inventory, where the controller is the sole
// target; an empty inventory now forces nothing and defers to ansible's own
// discovery (whose implicit localhost already runs under the venv python that
// ansible itself runs as).
func TestAnsibleInterpreterScopedToLocalConnection(t *testing.T) {
	const venvPython = "/data/cache/python/homelab/abc123/bin/python"

	inv := filepath.Join(t.TempDir(), "hosts.ini")
	if err := os.WriteFile(inv, []byte("[nodes]\nnode0\n"), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}

	extraVarsOf := func(t *testing.T, argv []string) (string, bool) {
		t.Helper()
		_, sink := runAnsible(t, executor.AnsibleOptions{
			Binary:         dumpExtraVars(t),
			Interpreter:    venvPython,
			PrivateDirRoot: t.TempDir(),
		}, argv)
		for _, l := range sink.msgs(executor.LevelInfo) {
			if body, ok := strings.CutPrefix(l, "extravars:"); ok {
				return body, true
			}
		}
		return "", false
	}

	t.Run("remote inventory does not force the venv interpreter", func(t *testing.T) {
		t.Parallel()
		body, forced := extraVarsOf(t, executor.AnsibleArgv("/p/site.yml", inv))
		if forced && strings.Contains(body, "ansible_python_interpreter") {
			t.Errorf("remote run forced the controller venv as a global interpreter: %s", body)
		}
		if strings.Contains(body, venvPython) {
			t.Errorf("controller venv path reached a remote run: %s", body)
		}
	})

	t.Run("explicit localhost inventory still forces the venv interpreter", func(t *testing.T) {
		t.Parallel()
		body, forced := extraVarsOf(t, executor.AnsibleArgv("/p/site.yml", "localhost,"))
		if !forced {
			t.Fatalf("explicit localhost run wrote no extra-vars; interpreter not forced")
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("extra-vars is not JSON: %q (%v)", body, err)
		}
		if got["ansible_python_interpreter"] != venvPython {
			t.Errorf("localhost interpreter = %q, want %q", got["ansible_python_interpreter"], venvPython)
		}
	})

	t.Run("empty inventory forces no interpreter and defers to discovery", func(t *testing.T) {
		t.Parallel()
		body, forced := extraVarsOf(t, executor.AnsibleArgv("/p/site.yml", ""))
		if forced && strings.Contains(body, "ansible_python_interpreter") {
			t.Errorf("empty-inventory run forced an interpreter instead of deferring: %s", body)
		}
		if strings.Contains(body, venvPython) {
			t.Errorf("controller venv path reached an empty-inventory run: %s", body)
		}
	})
}

func TestAnsiblePrivateDirIsRemovedOnFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	res, _ := runAnsible(t, executor.AnsibleOptions{
		Binary:         fakeAnsible(t, "exit 2\n"),
		VaultPassword:  []byte("pw"),
		PrivateDirRoot: root,
	}, executor.AnsibleArgv("/p/site.yml", ""))

	if res.ExitCode != 2 {
		t.Errorf("exit code = %d, want 2", res.ExitCode)
	}
	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read private dir root: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("private directory survived a failed run: %v", left)
	}
}

func TestAnsibleRejectsBadSpec(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opts executor.AnsibleOptions
		spec executor.Spec
	}{
		{
			name: "empty argv",
			spec: executor.Spec{Kind: executor.KindAnsible},
		},
		{
			name: "foreign kind",
			spec: executor.Spec{Kind: "shell", Argv: executor.AnsibleArgv("/p/site.yml", "")},
		},
		{
			name: "not ansible-playbook",
			spec: executor.Spec{Kind: executor.KindAnsible, Argv: []string{"/bin/sh", "-c", "true"}},
		},
		{
			name: "password with a newline",
			opts: executor.AnsibleOptions{VaultPassword: []byte("two\nlines")},
			spec: executor.Spec{Kind: executor.KindAnsible, Argv: executor.AnsibleArgv("/p/site.yml", "")},
		},
		{
			name: "vault id with a separator",
			opts: executor.AnsibleOptions{VaultID: "a@b", VaultPassword: []byte("pw")},
			spec: executor.Spec{Kind: executor.KindAnsible, Argv: executor.AnsibleArgv("/p/site.yml", "")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := tc.opts
			opts.Binary = fakeAnsible(t, "exit 0\n")
			opts.PrivateDirRoot = t.TempDir()

			var sink collector
			if _, err := executor.NewAnsible(opts).Run(context.Background(), tc.spec, &sink); err == nil {
				t.Fatal("run: want an error, got nil")
			}
			left, err := os.ReadDir(opts.PrivateDirRoot)
			if err != nil {
				t.Fatalf("read private dir root: %v", err)
			}
			if len(left) != 0 {
				t.Errorf("private directory survived a rejected spec: %v", left)
			}
		})
	}
}

// vaultedPlaybook writes a playbook whose only variable is vault-encrypted
// with password, and returns its path. It shells out to ansible-vault so the
// ciphertext is the real format, not a fixture.
func vaultedPlaybook(t *testing.T, dir, password, secret string) string {
	t.Helper()

	pwFile := filepath.Join(dir, "vault-pass")
	if err := os.WriteFile(pwFile, []byte(password+"\n"), 0o600); err != nil {
		t.Fatalf("write vault password: %v", err)
	}
	vars := filepath.Join(dir, "secret.yml")
	if err := os.WriteFile(vars, []byte("exocomp_canary: "+secret+"\n"), 0o600); err != nil {
		t.Fatalf("write vars: %v", err)
	}

	cmd := exec.Command("ansible-vault", "encrypt",
		"--vault-id", executor.DefaultVaultID+"@"+pwFile, vars)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ansible-vault encrypt: %v: %s", err, out)
	}
	if err := os.Remove(pwFile); err != nil {
		t.Fatalf("remove vault password: %v", err)
	}

	playbook := filepath.Join(dir, "site.yml")
	body := `- name: exocomp vault round trip
  hosts: localhost
  gather_facts: false
  vars_files:
    - secret.yml
  tasks:
    - name: print the vaulted value
      ansible.builtin.debug:
        msg: "canary is {{ exocomp_canary }}"
`
	if err := os.WriteFile(playbook, []byte(body), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	return playbook
}

func requireAnsible(t *testing.T) {
	t.Helper()
	for _, bin := range []string{executor.AnsibleBinary, "ansible-vault"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed: %v", bin, err)
		}
	}
}

func TestAnsibleRunsRealPlaybook(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real ansible")
	}
	requireAnsible(t)
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	root := t.TempDir()
	const secret = "canary-c0ffee-do-not-log"
	playbook := vaultedPlaybook(t, dir, "vault-password-under-test", secret)

	res, sink := runAnsible(t, executor.AnsibleOptions{
		VaultPassword:  []byte("vault-password-under-test"),
		PrivateDirRoot: root,
	}, executor.AnsibleArgv(playbook, ""))

	out := strings.Join(sink.msgs(""), "\n")
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", res.ExitCode, out)
	}
	// The vaulted value reaching the output is what proves the password file
	// was delivered; redacting it is the queue's job, not the driver's.
	if !strings.Contains(out, secret) {
		t.Errorf("vaulted value not decrypted:\n%s", out)
	}

	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read private dir root: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("private directory survived the run: %v", left)
	}
}

func TestAnsibleFailingPlaybookExitsNonZero(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real ansible")
	}
	requireAnsible(t)
	t.Setenv("HOME", t.TempDir())

	playbook := filepath.Join(t.TempDir(), "fail.yml")
	body := `- name: exocomp failure path
  hosts: localhost
  gather_facts: false
  tasks:
    - name: fail on purpose
      ansible.builtin.command: /bin/true
      changed_when: false
      failed_when: true
`
	if err := os.WriteFile(playbook, []byte(body), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	res, sink := runAnsible(t, executor.AnsibleOptions{PrivateDirRoot: t.TempDir()},
		executor.AnsibleArgv(playbook, ""))
	if res.ExitCode == 0 {
		t.Errorf("exit code = 0, want non-zero:\n%s", strings.Join(sink.msgs(""), "\n"))
	}
}

// A missing toolchain names the binary, the PATH searched and the three ways to
// fix it, instead of a bare exec error.
func TestAnsibleMissingBinaryNamesPathAndRemedy(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("PATH", empty)

	var sink collector
	_, err := executor.NewAnsible(executor.AnsibleOptions{PrivateDirRoot: t.TempDir()}).Run(context.Background(),
		executor.Spec{Kind: executor.KindAnsible, Argv: executor.AnsibleAdhocArgv("localhost", "", "ping", "")}, &sink)
	if err == nil {
		t.Fatal("run: want an error, got nil")
	}
	for _, want := range []string{"ansible not found on PATH", empty, "Python manifest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}
