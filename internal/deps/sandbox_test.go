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
		Binary:       "/opt/nono",
		DataDir:      data,
		VenvDir:      cacheSource,
		AllowDomains: PythonIndexHosts(),
	})
	full, err := sb.Wrap([]string{"/usr/bin/uv", "pip", "install", "jmespath"})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	if got, want := full[:4], []string{"/opt/nono", "run", "--silent", "--no-audit"}; !slices.Equal(got, want) {
		t.Errorf("launcher prefix = %v, want %v", got, want)
	}
	if slices.Contains(full, "--block-net") {
		t.Errorf("resolver in proxy mode still passes --block-net, which nono refuses with --allow-domain: %v", full)
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
	if got, want := flagValues(full, "--allow-domain"), []string{"files.pythonhosted.org", "pypi.org"}; !slices.Equal(got, want) {
		t.Errorf("resolver --allow-domain = %v, want %v", got, want)
	}
	if got := flagValues(full, "--allow-connect-port"); len(got) != 0 {
		t.Errorf("resolver carries raw connect ports %v next to the proxy", got)
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

// TestResolverSandboxGrantsThePythonInstallDir: both resolvers start a Python
// interpreter and uv lists the install dir to find it, so the resolver profile
// reads it, and drops a value inside the data dir.
func TestResolverSandboxGrantsThePythonInstallDir(t *testing.T) {
	python := t.TempDir()
	data := t.TempDir()
	resolved, err := filepath.EvalSymlinks(python)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wrap := func() []string {
		t.Helper()
		sb := NewResolverSandbox(ResolverSandboxOptions{Binary: "/opt/nono", DataDir: data, VenvDir: filepath.Join(data, "cache", "v")})
		full, err := sb.Wrap([]string{"/usr/bin/uv", "venv"})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		return flagValues(full, "--read")
	}

	t.Setenv(executor.PythonInstallEnv, python)
	if !slices.Contains(wrap(), resolved) {
		t.Errorf("resolver read grants %v lack the Python install dir %s", wrap(), resolved)
	}

	inside := filepath.Join(data, "run")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv(executor.PythonInstallEnv, inside)
	insideResolved, err := filepath.EvalSymlinks(inside)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := wrap(); slices.Contains(got, inside) || slices.Contains(got, insideResolved) {
		t.Errorf("resolver read grants %v reach inside the data dir via %s", got, inside)
	}
}

// TestResolverSandboxOfflineReachesNoHost: an offline resolve is cache-only, so
// its profile keeps --block-net and names no domain.
func TestResolverSandboxOfflineReachesNoHost(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	sb := NewResolverSandbox(ResolverSandboxOptions{
		Binary: "/opt/nono", DataDir: data, VenvDir: filepath.Join(data, "cache", "v"),
		AllowDomains: PythonIndexHosts(), Offline: true,
	})
	full, err := sb.Wrap([]string{"/usr/bin/uv", "pip", "install", "jmespath"})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if !slices.Contains(full, "--block-net") {
		t.Errorf("offline resolver does not block the network: %v", full)
	}
	if got := flagValues(full, "--allow-domain"); len(got) != 0 {
		t.Errorf("offline resolver names domains %v", got)
	}
}

// TestResolverHostsAreNamedHosts: the defaults each resolver ships are plain host
// names, never a wildcard, because the proxy filters by name and a wildcard lets a
// name that resolves to loopback or the LAN through.
func TestResolverHostsAreNamedHosts(t *testing.T) {
	t.Parallel()

	for name, hosts := range map[string][]string{"uv": PythonIndexHosts(), "ansible-galaxy": GalaxyHosts()} {
		if len(hosts) == 0 {
			t.Errorf("%s has no default hosts", name)
		}
		for _, h := range hosts {
			if strings.ContainsAny(h, "*/: ") {
				t.Errorf("%s default host %q is not a plain host name", name, h)
			}
		}
	}
}

// TestResolverSandboxReachesOnlyTheNamedHosts: the resolver's network is the
// hosts it is given and nothing wider. A resolver given none keeps --block-net,
// so an unconfigured caller fails closed instead of opening the network.
func TestResolverSandboxReachesOnlyTheNamedHosts(t *testing.T) {
	t.Parallel()

	data := t.TempDir()
	build := func(hosts []string) []string {
		t.Helper()
		sb := NewResolverSandbox(ResolverSandboxOptions{
			Binary: "/opt/nono", DataDir: data, VenvDir: filepath.Join(data, "cache", "v"), AllowDomains: hosts,
		})
		full, err := sb.Wrap([]string{"/usr/bin/uv", "venv"})
		if err != nil {
			t.Fatalf("wrap: %v", err)
		}
		return full
	}

	full := build([]string{"index.example.net", "pypi.org"})
	if slices.Contains(full, "--block-net") {
		t.Errorf("proxy mode still passes --block-net: %v", full)
	}
	if got, want := flagValues(full, "--allow-domain"), []string{"index.example.net", "pypi.org"}; !slices.Equal(got, want) {
		t.Errorf("--allow-domain = %v, want %v", got, want)
	}

	none := build(nil)
	if !slices.Contains(none, "--block-net") || len(flagValues(none, "--allow-domain")) != 0 {
		t.Errorf("a resolver given no hosts is not network-blocked: %v", none)
	}
}
