package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

func TestAnsibleArgvPassthrough(t *testing.T) {
	t.Parallel()

	// Operator flags land before the playbook so ansible reads them as options,
	// and after the inventory so the inventory pairing stays intact.
	got := executor.AnsibleArgv("/p/site.yml", "/p/hosts.ini",
		"--check", "--diff", "--limit", "web", "--tags", "deploy")
	want := []string{
		"ansible-playbook", "--inventory", "/p/hosts.ini",
		"--check", "--diff", "--limit", "web", "--tags", "deploy",
		"/p/site.yml",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("AnsibleArgv = %v\nwant %v", got, want)
	}
	// The playbook is always last, even with passthrough.
	if got[len(got)-1] != "/p/site.yml" {
		t.Fatalf("playbook is not the last argument: %v", got)
	}

	// Empty inventory adds no --inventory at all: the passthrough sits directly
	// after the binary and ansible defers to its own inventory discovery.
	local := executor.AnsibleArgv("/p/site.yml", "", "--check")
	wantLocal := []string{"ansible-playbook", "--check", "/p/site.yml"}
	if !slices.Equal(local, wantLocal) {
		t.Fatalf("empty-inventory AnsibleArgv = %v\nwant %v", local, wantLocal)
	}
}

// TestAnsibleMultipleVaultIDs proves two vault-ids each reach ansible as their
// own --vault-id label@file, with the imported password readable in the file —
// the "dev and prod vaults in one repo" and "import an existing password" fixes.
func TestAnsibleMultipleVaultIDs(t *testing.T) {
	t.Parallel()

	bin := fakeAnsible(t, `
for a in "$@"; do
	echo "arg:$a"
	case "$a" in
		*@*) f="${a#*@}"; [ -f "$f" ] && echo "vaultfile:${a%@*}:$(cat "$f")" ;;
	esac
done
`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var sink collector
	res, err := executor.NewAnsible(executor.AnsibleOptions{
		Binary:         bin,
		PrivateDirRoot: t.TempDir(),
		VaultID:        "dev",
		VaultPassword:  []byte("dev-derived"),
		Vaults: []executor.VaultEntry{
			{ID: "prod", Password: []byte("prod-imported")},
		},
	}).Run(ctx, executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv("/p/site.yml", ""),
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}

	lines := sink.msgs(executor.LevelInfo)
	if !slices.Contains(lines, "vaultfile:dev:dev-derived") {
		t.Errorf("dev vault-id not delivered with its password: %v", lines)
	}
	if !slices.Contains(lines, "vaultfile:prod:prod-imported") {
		t.Errorf("prod vault-id not delivered with its imported password: %v", lines)
	}

	// Both vault files live under the private dir, one per id.
	var ids []string
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "arg:"); ok && strings.HasPrefix(rest, "-") {
			continue
		}
		if id, _, ok := strings.Cut(strings.TrimPrefix(l, "vaultfile:"), ":"); ok && strings.HasPrefix(l, "vaultfile:") {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"dev", "prod"}) {
		t.Fatalf("vault ids delivered = %v, want [dev prod]", ids)
	}
}

func TestAnsibleDuplicateVaultIDRejected(t *testing.T) {
	t.Parallel()
	bin := fakeAnsible(t, "exit 0\n")
	_, err := executor.NewAnsible(executor.AnsibleOptions{
		Binary:         bin,
		PrivateDirRoot: t.TempDir(),
		VaultID:        "prod",
		VaultPassword:  []byte("a"),
		Vaults:         []executor.VaultEntry{{ID: "prod", Password: []byte("b")}},
	}).Run(context.Background(), executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv("/p/site.yml", ""),
	}, &collector{})
	if err == nil {
		t.Fatal("naming one vault id twice should fail")
	}
}

// TestAnsibleRunsFromCheckoutDir proves the job's working directory is honoured
// so a repo's own ./ansible.cfg loads (the expert-review must-fix): the fake
// ansible reports its cwd, which must be the checkout the spec named.
func TestAnsibleRunsFromCheckoutDir(t *testing.T) {
	t.Parallel()
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "ansible.cfg"),
		[]byte("[defaults]\nstdout_callback = yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := fakeAnsible(t, `echo "cwd:$(pwd)"
[ -f ./ansible.cfg ] && echo "cfg:present"
`)
	var sink collector
	res, err := executor.NewAnsible(executor.AnsibleOptions{
		Binary:         bin,
		PrivateDirRoot: t.TempDir(),
	}).Run(context.Background(), executor.Spec{
		Kind: executor.KindAnsible,
		Dir:  checkout,
		Argv: executor.AnsibleArgv(filepath.Join(checkout, "site.yml"), ""),
	}, &sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}
	lines := sink.msgs(executor.LevelInfo)
	// macOS/Linux symlink the temp dir; compare resolved forms.
	wantCwd, _ := filepath.EvalSymlinks(checkout)
	var gotCwd string
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "cwd:"); ok {
			gotCwd, _ = filepath.EvalSymlinks(v)
		}
	}
	if gotCwd != wantCwd {
		t.Fatalf("ansible ran in %q, want the checkout %q (its ./ansible.cfg would not load)", gotCwd, wantCwd)
	}
	if !slices.Contains(lines, "cfg:present") {
		t.Errorf("the checkout's ./ansible.cfg was not visible from the run cwd: %v", lines)
	}
}
