package deps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// TestResolverSandboxWrapsAndDeniesDataDir is the hermetic half of the
// resolver-confinement check: the launcher the resolver runs under wraps the
// uv argv, grants the venv cache and HTTPS, and refuses to build a profile
// whose grants would reach the data dir, so a resolver can never be handed
// the keys.
func TestResolverSandboxWrapsAndDeniesDataDir(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real nono sandbox")
	}
	t.Parallel()

	data := t.TempDir()
	cacheSource := filepath.Join(t.TempDir(), "src")
	sb := NewResolverSandbox(ResolverSandboxOptions{
		Binary:  "/opt/nono",
		DataDir: data,
		VenvDir: cacheSource,
	})
	full, err := sb.Wrap([]string{"/usr/bin/uv", "pip", "install", "jmespath"})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	if got, want := full[:5], []string{"/opt/nono", "run", "--silent", "--no-audit", "--block-net"}; !slices.Equal(got, want) {
		t.Errorf("launcher prefix = %v, want %v", got, want)
	}
	sep := slices.Index(full, "--")
	if sep < 0 {
		t.Fatalf("argv has no -- separator: %v", full)
	}
	if got, want := full[sep+1:], []string{"/usr/bin/uv", "pip", "install", "jmespath"}; !slices.Equal(got, want) {
		t.Errorf("wrapped argv = %v, want %v", got, want)
	}
	if !slices.Contains(flagValues(full, "--allow"), cacheSource) {
		t.Errorf("venv cache dir not granted write: %v", full)
	}
	if !slices.Contains(flagValues(full, "--allow-connect-port"), "443") {
		t.Errorf("resolver not granted HTTPS egress: %v", full)
	}
	// The data dir, and every key beneath it, must be reachable through no grant.
	for _, grant := range slices.Concat(flagValues(full, "--allow"), flagValues(full, "--read"), flagValues(full, "--read-file")) {
		if grant == data || strings.HasPrefix(grant, data+string(filepath.Separator)) {
			t.Errorf("grant %q reaches the data dir %q", grant, data)
		}
	}

	// A launcher whose venv grant IS the data dir is refused outright, so a
	// misconfiguration cannot hand the resolver the database and the keys.
	bad := NewResolverSandbox(ResolverSandboxOptions{DataDir: data, VenvDir: data})
	if _, err := bad.Wrap([]string{"/usr/bin/uv", "venv"}); err == nil {
		t.Fatal("a resolver profile granting the data dir was accepted")
	}
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

// exfilUV writes a uv stand-in that models a poisoned manifest: on the pip
// install step it tries to read exfil (a file inside the data dir). If it can,
// it prints EXFIL-OK and exits 0 — the daemon-context exploit the sandbox
// exists to close; if the read is refused it exits non-zero. On the venv step
// it lays down the
// venv skeleton EnsureVenv checks for, so an *allowed* resolve still succeeds.
func exfilUV(t *testing.T, exfil string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "uv")
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    venv) mode=venv ;;
    install) mode=install ;;
  esac
  last="$a"
done
if [ "$mode" = venv ]; then
  mkdir -p "$last/bin" || exit 1
  mkdir -p "$last/lib/python3.14/site-packages" || exit 1
  : > "$last/bin/python"; chmod +x "$last/bin/python"
  : > "$last/bin/ansible-playbook"; chmod +x "$last/bin/ansible-playbook"
  exit 0
fi
if [ "$mode" = install ]; then
  if cat "` + exfil + `" >/dev/null 2>&1; then
    echo "EXFIL-OK"; exit 0
  fi
  echo "exfil refused" >&2; exit 66
fi
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write exfil uv: %v", err)
	}
	return path
}

// TestResolverSandboxDeniesDataDirRead runs the exploit-then-refusal
// against the real launcher: a manifest whose install step reads the data dir
// succeeds when the resolver runs in the daemon context (no sandbox) and fails
// when it runs under the resolver sandbox, because the read is denied.
func TestResolverSandboxDeniesDataDirRead(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real nono sandbox")
	}
	requireNono(t)

	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "host.key"), []byte("SEALED-DEK\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exfil := filepath.Join(data, "host.key")

	cache := t.TempDir()
	project := t.TempDir()
	m := manifestFor(t, project, "jmespath==1.1.0\n")
	uv := exfilUV(t, exfil)

	// Before: no sandbox — the resolver runs in the daemon context and reads the
	// sealed key straight out of the data dir. This is what the sandbox exists
	// to prevent.
	unconfined := VenvSpec{CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv, Offline: true}
	if _, err := EnsureVenv(context.Background(), unconfined); err != nil {
		t.Fatalf("unconfined resolve failed, so the exploit control is not proven: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "demo", m.Hash, readyMarker)); err != nil {
		t.Fatalf("unconfined resolve did not complete the venv (exfil control broken): %v", err)
	}

	// After: the same manifest under the resolver sandbox. The data-dir read is
	// denied by the kernel, uv exits non-zero, and EnsureVenv fails closed —
	// leaving no venv behind.
	confinedCache := t.TempDir()
	confined := VenvSpec{
		CacheRoot: confinedCache,
		Source:    "demo",
		Manifest:  m,
		UVBinary:  uv,
		Offline:   true,
		Sandbox: NewResolverSandbox(ResolverSandboxOptions{
			DataDir:   data,
			VenvDir:   filepath.Join(confinedCache, "demo"),
			ReadPaths: []string{filepath.Dir(uv), project},
			Offline:   true,
		}),
	}
	if _, err := EnsureVenv(context.Background(), confined); err == nil {
		t.Fatal("resolver read the data dir under the sandbox — denial not enforced")
	}
	if _, err := os.Stat(filepath.Join(confinedCache, "demo", m.Hash)); !os.IsNotExist(err) {
		t.Errorf("a denied resolve left a venv behind (stat err = %v)", err)
	}
}

// requireNono skips a test when the real launcher cannot enforce a file-read
// denial on this host, so make check stays green where nono or Landlock is
// absent while the pre-commit host (which has both) runs the real assertion.
// The gate is functional: it stands up a denied file and a granted work dir and
// requires nono to actually refuse the read, so a nono that is present but not
// enforcing does not make the test above pass vacuously.
func requireNono(t *testing.T) {
	t.Helper()
	nono, err := exec.LookPath(executor.NonoBinary)
	if err != nil {
		t.Skipf("%s not on PATH", executor.NonoBinary)
	}
	deniedDir := t.TempDir()
	denied := filepath.Join(deniedDir, "secret")
	if err := os.WriteFile(denied, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	cmd := exec.Command(nono, "run", "--silent", "--no-audit", "--block-net",
		"--allow", work, "--", "/bin/cat", denied)
	cmd.Dir = work
	if err := cmd.Run(); err == nil {
		t.Skip("nono did not enforce a file-read denial here; skipping the real-sandbox assertion")
	}
}
