package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/deps"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// fakeGalaxyBinary writes an ansible-galaxy stand-in that records every
// invocation's argv to counter and lays down a fake installed collection under
// the -p target, so a provisioned dir is distinguishable from an empty one. It
// exercises the resolve path offline: what matters is whether ansible-galaxy is
// invoked at all and with which -p, never what it resolves.
func fakeGalaxyBinary(t *testing.T, bin, counter string) {
	t.Helper()
	path := filepath.Join(bin, "ansible-galaxy")
	script := `#!/bin/sh
echo "$@" >> "` + counter + `"
for a in "$@"; do target="$a"; done
mkdir -p "$target/ansible_collections/community/general" || exit 1
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ansible-galaxy: %v", err)
	}
}

// fakeAnsiblePlaybook writes an ansible-playbook stand-in that dumps its whole
// environment to probe and exits 0.
func fakeAnsiblePlaybook(t *testing.T, bin, probe string) {
	t.Helper()
	path := filepath.Join(bin, "ansible-playbook")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nenv > "+probe+"\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake ansible-playbook: %v", err)
	}
}

// invocationCount is how many times the fake ansible-galaxy has run.
func invocationCount(t *testing.T, counter string) int {
	t.Helper()
	body, err := os.ReadFile(counter)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read invocations: %v", err)
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// TestResolveSourceCollectionsProvisionsPerSourceDir is the Decision 0015 resolve
// path: a playbook whose directory carries requirements.yml resolves a
// hash-keyed per-source collections dir, invoking the fake ansible-galaxy
// exactly once, and a second resolve of the same requirements is a cache hit
// (Created false, no further invocation).
func TestResolveSourceCollectionsProvisionsPerSourceDir(t *testing.T) {
	forceDisabledTier(t)
	binDir := t.TempDir()
	counter := filepath.Join(t.TempDir(), "galaxy-invocations")
	fakeGalaxyBinary(t, binDir, counter)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	project := t.TempDir()
	writeFile(t, filepath.Join(project, "requirements.yml"), "collections:\n  - community.sops\n")
	play := filepath.Join(project, "site.yml")
	dataDir := t.TempDir()

	manifest, err := deps.DiscoverGalaxy(project)
	if err != nil {
		t.Fatalf("discover galaxy: %v", err)
	}

	first, err := resolveSourceCollections(context.Background(), dataDir, play)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if !first.Created {
		t.Fatal("first resolve did not provision")
	}
	want := filepath.Join(dataDir, collectionsCacheDir, deps.SourceKey(manifest.Root), manifest.Hash)
	if first.Dir != want {
		t.Errorf("Dir = %s, want %s", first.Dir, want)
	}
	if got := invocationCount(t, counter); got != 1 {
		t.Errorf("ansible-galaxy invoked %d times on the first resolve, want 1", got)
	}

	second, err := resolveSourceCollections(context.Background(), dataDir, play)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if second.Created {
		t.Error("a cache hit re-invoked ansible-galaxy")
	}
	if second.Dir != first.Dir {
		t.Errorf("a cache hit moved the dir %s -> %s", first.Dir, second.Dir)
	}
	if got := invocationCount(t, counter); got != 1 {
		t.Errorf("ansible-galaxy invoked %d times total after a cache hit, want 1", got)
	}
}

// TestResolveSourceCollectionsSkipsAdhoc: an ad-hoc run names no playbook, so
// ansiblePlaybookPath returns "" and collections resolve skips it, with no
// ansible-galaxy invocation. The venv resolve is the exception: an ad-hoc run
// from a source finds it through ansibleVenvAnchor.
func TestResolveSourceCollectionsSkipsAdhoc(t *testing.T) {
	forceDisabledTier(t)
	binDir := t.TempDir()
	counter := filepath.Join(t.TempDir(), "galaxy-invocations")
	fakeGalaxyBinary(t, binDir, counter)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	argv := executor.AnsibleAdhocArgv("localhost", "", "ping", "")
	if pb := ansiblePlaybookPath(argv); pb != "" {
		t.Fatalf("an adhoc argv resolved a playbook path %q", pb)
	}
	c, err := resolveSourceCollections(context.Background(), t.TempDir(), ansiblePlaybookPath(argv))
	if err != nil {
		t.Fatalf("adhoc resolve: %v", err)
	}
	if c.Dir != "" || c.Created {
		t.Errorf("an adhoc resolve produced %+v, want a zero Collections", c)
	}
	if got := invocationCount(t, counter); got != 0 {
		t.Errorf("ansible-galaxy invoked %d times for an adhoc run, want 0", got)
	}
}

// TestResolveSourceCollectionsNoManifestFallsBackToShared: a play whose
// directory declares no requirements.yml returns a zero Collections so the run
// falls back to the shared cache/collections dir, and nothing is provisioned.
func TestResolveSourceCollectionsNoManifestFallsBackToShared(t *testing.T) {
	forceDisabledTier(t)
	binDir := t.TempDir()
	counter := filepath.Join(t.TempDir(), "galaxy-invocations")
	fakeGalaxyBinary(t, binDir, counter)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	project := t.TempDir()
	play := filepath.Join(project, "site.yml")
	c, err := resolveSourceCollections(context.Background(), t.TempDir(), play)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.Dir != "" || c.Created {
		t.Errorf("a manifest-less resolve produced %+v, want a zero Collections", c)
	}
	if got := invocationCount(t, counter); got != 0 {
		t.Errorf("ansible-galaxy invoked %d times for a source with no manifest, want 0", got)
	}
}

// TestAnsibleCollectionsPathJoinsPerSourceAheadOfShared pins the env value the
// driver builds: per-source collections ahead of the shared fallback when a
// source resolved collections, the shared dir alone otherwise (Decision 0015).
func TestAnsibleCollectionsPathJoinsPerSourceAheadOfShared(t *testing.T) {
	data := t.TempDir()
	shared := filepath.Join(data, collectionsCacheDir)
	collections := deps.Collections{Dir: filepath.Join(shared, "demo-1a2b3c4d", "deadbeef")}

	if got, want := ansibleCollectionsPath(data, collections), collections.Dir+string(os.PathListSeparator)+shared; got != want {
		t.Errorf("ansibleCollectionsPath = %q, want %q", got, want)
	}
	if got := ansibleCollectionsPath(data, deps.Collections{}); got != shared {
		t.Errorf("ansibleCollectionsPath without a per-source dir = %q, want the shared dir %q", got, shared)
	}
}

// TestAnsibleDriverRunNamesPerSourceCollectionsFirst proves the run path wires
// the resolved per-source collections into ANSIBLE_COLLECTIONS_PATH ahead of
// the shared fallback (Decision 0015), and that a second run of an unchanged source
// is a cache hit: no further ansible-galaxy, no repeat resolve event.
func TestAnsibleDriverRunNamesPerSourceCollectionsFirst(t *testing.T) {
	forceDisabledTier(t)
	dir := initForSource(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	invocations := filepath.Join(t.TempDir(), "galaxy-invocations")
	probe := filepath.Join(t.TempDir(), "child.env")
	binDir := t.TempDir()
	fakeGalaxyBinary(t, binDir, invocations)
	fakeAnsiblePlaybook(t, binDir, probe)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	project := t.TempDir()
	writeFile(t, filepath.Join(project, "requirements.yml"), "collections:\n  - community.sops\n")
	play := filepath.Join(project, "site.yml")
	writeFile(t, play, "- hosts: all\n  tasks: []\n")

	manifest, err := deps.DiscoverGalaxy(project)
	if err != nil {
		t.Fatalf("discover galaxy: %v", err)
	}
	perSource := filepath.Join(dir, collectionsCacheDir, deps.SourceKey(manifest.Root), manifest.Hash)
	shared := filepath.Join(dir, collectionsCacheDir)
	wantPath := perSource + string(os.PathListSeparator) + shared

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	run := func() int64 {
		t.Helper()
		drv := ansibleJobDriver{store: st, dataDir: dir}
		q := queue.New(st, drv, queue.Options{Workers: 1, PollInterval: 20 * time.Millisecond})
		job, err := q.Enqueue(ctx, queue.Spec{
			Runbook: "playbook",
			Kind:    executor.KindAnsible,
			Trigger: "manual",
			Argv:    executor.AnsibleArgv(play, ""),
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := q.Drain(ctx); err != nil {
			t.Fatalf("drain: %v", err)
		}
		return job.ID
	}

	job1 := run()
	childEnv, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read child env: %v", err)
	}
	if !strings.Contains(string(childEnv), "ANSIBLE_COLLECTIONS_PATH="+wantPath) {
		t.Fatalf("child env missing the per-source collections path:\n%s", childEnv)
	}
	if got := invocationCount(t, invocations); got != 1 {
		t.Fatalf("ansible-galaxy invoked %d times on the first run, want 1", got)
	}
	if events := jobEventText(t, job1); !strings.Contains(events, "resolved source collections into "+perSource) {
		t.Errorf("first run's events missing the collections resolve event:\n%s", events)
	}

	job2 := run()
	if got := invocationCount(t, invocations); got != 1 {
		t.Fatalf("ansible-galaxy invoked %d times total after a cache hit, want 1", got)
	}
	if events := jobEventText(t, job2); strings.Contains(events, "resolved source collections into") {
		t.Errorf("a cache-hit run emitted a collections resolve event:\n%s", events)
	}
}

// fakeNono writes a nono stand-in that records the argv it was handed to log
// and then execs the payload after the `--` separator, standing in for the
// launcher's grant application without a kernel. It makes the resolver
// profile's grants observable the way the executor's capturingDriver observes
// its launcher argv.
func fakeNono(t *testing.T, bin, log string) {
	t.Helper()
	path := filepath.Join(bin, "nono")
	script := `#!/bin/sh
echo "$@" >> "` + log + `"
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
[ "$#" -gt 0 ] && shift
exec "$@"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake nono: %v", err)
	}
}

// TestResolveSourceCollectionsSandboxGrantsVenvGalaxy proves the sandboxed
// collections resolve grants the ansible-galaxy that actually executes: when
// the source resolved a per-source venv, its bin/ansible-galaxy is the
// resolver (Decision 0015), so the resolver profile must carry a read grant on that
// venv binary — the image galaxy grant alone would deny the venv one that
// runs. A fake nono records the launcher argv, mirroring the executor's
// sandbox-grant tests.
func TestResolveSourceCollectionsSandboxGrantsVenvGalaxy(t *testing.T) {
	resetSandboxDecision(t)
	old := resolverExtraHosts
	resolverExtraHosts = []string{"index.example.net"}
	t.Cleanup(func() { resolverExtraHosts = old })
	sandbox.requested = executor.TierEnforcing
	sandbox.result = executor.ProbeResult{Requested: executor.TierEnforcing, Effective: executor.TierEnforcing, ProxyEnforced: true}
	sandbox.once.Do(func() {})

	nonoLog := filepath.Join(t.TempDir(), "nono-argv")
	binDir := t.TempDir()
	fakeNono(t, binDir, nonoLog)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dataDir := t.TempDir()
	project := t.TempDir()
	writeFile(t, filepath.Join(project, "requirements.txt"), "jmespath==1.1.0\n")
	writeFile(t, filepath.Join(project, "requirements.yml"), "collections:\n  - community.sops\n")
	play := filepath.Join(project, "site.yml")

	// Seed the per-source venv as a cache hit (no uv runs) with an executable
	// ansible-galaxy sibling, the resolver B2 ensures the sandbox grants. Its
	// collection-install body matches the fake galaxy so a provision succeeds.
	py, err := deps.DiscoverPython(project)
	if err != nil {
		t.Fatalf("discover python: %v", err)
	}
	source := deps.SourceKey(py.Root)
	venv := filepath.Join(dataDir, "cache", "python", source, py.Hash)
	venvBin := filepath.Join(venv, "bin")
	if err := os.MkdirAll(venvBin, 0o700); err != nil {
		t.Fatalf("mkdir venv bin: %v", err)
	}
	for tool, body := range map[string]string{
		"python":           "#!/bin/sh\nexit 0\n",
		"ansible-playbook": "#!/bin/sh\nexit 0\n",
		"ansible-galaxy":   "#!/bin/sh\nfor a in \"$@\"; do target=\"$a\"; done\nmkdir -p \"$target/ansible_collections/community/general\" || exit 1\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(venvBin, tool), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", tool, err)
		}
	}
	writeFile(t, filepath.Join(venv, ".exocomp-venv-ready"), py.Hash+"\n")

	c, err := resolveSourceCollections(context.Background(), dataDir, play)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !c.Created {
		t.Fatal("expected a fresh collections provision through the resolver sandbox")
	}

	launcher := strings.TrimSpace(readFile(t, nonoLog))
	if !strings.Contains(launcher, "collection install") {
		t.Fatalf("the launcher argv is not the galaxy install:\n%s", launcher)
	}
	// The venv's ansible-galaxy executed, so the profile must have granted it.
	// ToolGrants names the bin dir and the venv root for the interpreted tool.
	if !strings.Contains(launcher, "--read "+venv) {
		t.Errorf("resolver profile does not read-grant the venv ansible-galaxy (%s):\n%s", venv, launcher)
	}
	if !strings.Contains(launcher, "--read "+venvBin) {
		t.Errorf("resolver profile does not read-grant the venv bin dir:\n%s", launcher)
	}
	// The collections resolver reaches the Galaxy hosts and an operator's extra,
	// through the proxy and with no --block-net, and not uv's hosts.
	if got, want := launcherDomains(launcher), []string{
		"ansible-galaxy-ng.s3.dualstack.us-east-1.amazonaws.com", "galaxy.ansible.com", "index.example.net",
	}; !slices.Equal(got, want) {
		t.Errorf("galaxy resolver --allow-domain = %v, want %v\n%s", got, want, launcher)
	}
	if strings.Contains(launcher, "--block-net") {
		t.Errorf("galaxy resolver in proxy mode still passes --block-net:\n%s", launcher)
	}
}

// launcherDomains returns the sorted --allow-domain values in a recorded launcher argv.
func launcherDomains(launcher string) []string {
	var out []string
	fields := strings.Fields(launcher)
	for i := range fields {
		if fields[i] == "--allow-domain" && i+1 < len(fields) {
			out = append(out, fields[i+1])
		}
	}
	slices.Sort(out)
	return out
}

// TestResolveProjectVenvSandboxReachesOnlyIndexHosts: the uv resolver is handed
// uv's index hosts plus an operator's extra, through the proxy and with no
// --block-net. A fake nono records the launcher argv, as for the collections
// resolver above.
func TestResolveProjectVenvSandboxReachesOnlyIndexHosts(t *testing.T) {
	resetSandboxDecision(t)
	sandbox.requested = executor.TierEnforcing
	sandbox.result = executor.ProbeResult{Requested: executor.TierEnforcing, Effective: executor.TierEnforcing, ProxyEnforced: true}
	sandbox.once.Do(func() {})
	old := resolverExtraHosts
	resolverExtraHosts = []string{"index.example.net"}
	t.Cleanup(func() { resolverExtraHosts = old })

	nonoLog := filepath.Join(t.TempDir(), "nono-argv")
	binDir := t.TempDir()
	fakeNono(t, binDir, nonoLog)
	fakeVenvUV(t, binDir, filepath.Join(t.TempDir(), "child.env"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	project := t.TempDir()
	writeFile(t, filepath.Join(project, "requirements.txt"), "jmespath==1.1.0\n")
	if _, err := resolveProjectVenv(context.Background(), t.TempDir(), filepath.Join(project, "site.yml")); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	launcher := strings.TrimSpace(readFile(t, nonoLog))
	if launcher == "" {
		t.Fatal("the resolver did not run under the launcher")
	}
	for _, line := range strings.Split(launcher, "\n") {
		if got, want := launcherDomains(line), []string{"files.pythonhosted.org", "index.example.net", "pypi.org"}; !slices.Equal(got, want) {
			t.Errorf("uv resolver --allow-domain = %v, want %v\n%s", got, want, line)
		}
		if strings.Contains(line, "--block-net") {
			t.Errorf("uv resolver in proxy mode still passes --block-net:\n%s", line)
		}
	}
}

// TestResolveSourceCollectionsSandboxGrantsLocalCollection is the live-bug
// regression: a requirements file naming a collection on the local disk
// (type: dir) outside the project root made ansible-galaxy fail with EACCES
// reading its galaxy.yml, because the resolver sandbox granted only the project
// root. The local artifact path must ride in the profile's read grants.
func TestResolveSourceCollectionsSandboxGrantsLocalCollection(t *testing.T) {
	resetSandboxDecision(t)
	sandbox.requested = executor.TierEnforcing
	sandbox.result = executor.ProbeResult{Requested: executor.TierEnforcing, Effective: executor.TierEnforcing, ProxyEnforced: true}
	sandbox.once.Do(func() {})

	nonoLog := filepath.Join(t.TempDir(), "nono-argv")
	binDir := t.TempDir()
	fakeNono(t, binDir, nonoLog)
	fakeGalaxyBinary(t, binDir, filepath.Join(t.TempDir(), "galaxy-invocations"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dataDir := t.TempDir()
	project := t.TempDir()
	local := filepath.Join(t.TempDir(), "unifi_network")
	if err := os.MkdirAll(local, 0o700); err != nil {
		t.Fatalf("mkdir local collection: %v", err)
	}
	writeFile(t, filepath.Join(local, "galaxy.yml"), "namespace: abn\nname: unifi_network\n")
	writeFile(t, filepath.Join(project, "requirements.yml"),
		"collections:\n  - name: "+local+"\n    type: dir\n")
	play := filepath.Join(project, "site.yml")

	c, err := resolveSourceCollections(context.Background(), dataDir, play)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !c.Created {
		t.Fatal("expected a fresh collections provision through the resolver sandbox")
	}

	launcher := strings.TrimSpace(readFile(t, nonoLog))
	if !strings.Contains(launcher, "collection install") {
		t.Fatalf("the launcher argv is not the galaxy install:\n%s", launcher)
	}
	if !strings.Contains(launcher, "--read "+local) {
		t.Errorf("resolver profile does not read-grant the local collection (%s):\n%s", local, launcher)
	}
}

// TestResolveSourceCollectionsSandboxProvisionsWithoutOverlappingNonoState is
// the live-bug regression: the collections resolver granted its galaxy cache
// dir write and redirected the resolver's HOME onto it, and nono refused to
// launch because the recursive grant covered its own protected state root
// under the granted HOME ("Refusing to grant ... overlaps protected nono
// state root .../.galaxy-cache/.local/state/nono"). A resolve that never
// reaches ansible-galaxy under the enforcing tier is the bug; this test
// requires the provision to succeed, which only happens when the launcher
// started and the fake galaxy ran confined. A real nono is required — a fake
// launcher cannot reproduce a refusal only the launcher itself makes.
func TestResolveSourceCollectionsSandboxProvisionsWithoutOverlappingNonoState(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real nono sandbox")
	}
	if _, err := exec.LookPath(executor.NonoBinary); err != nil {
		t.Skipf("%s not on PATH", executor.NonoBinary)
	}
	resetSandboxDecision(t)
	sandbox.requested = executor.TierEnforcing
	sandbox.result = executor.ProbeResult{Requested: executor.TierEnforcing, Effective: executor.TierEnforcing, ProxyEnforced: true}
	sandbox.once.Do(func() {})

	binDir := t.TempDir()
	fakeGalaxyBinary(t, binDir, filepath.Join(t.TempDir(), "galaxy-invocations"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	project := t.TempDir()
	writeFile(t, filepath.Join(project, "requirements.yml"), "collections:\n  - community.sops\n")
	play := filepath.Join(project, "site.yml")
	dataDir := t.TempDir()

	c, err := resolveSourceCollections(context.Background(), dataDir, play)
	if err != nil {
		t.Fatalf("resolve under the enforcing tier failed: %v", err)
	}
	if !c.Created {
		t.Fatal("expected a fresh collections provision through the resolver sandbox")
	}
	if _, statErr := os.Stat(filepath.Join(c.Dir, ".exocomp-galaxy-ready")); statErr != nil {
		t.Fatalf("provisioned collections dir missing its ready marker: %v", statErr)
	}
	// The resolver HOME must never be granted: nono keeps its protected state
	// root under $HOME and a grant covering it would refuse the launch above.
	// The galaxy cache root's home and its granted children are created for
	// the resolver by the daemon.
	for _, d := range []string{
		filepath.Join(dataDir, "cache", "galaxy", "home"),
		filepath.Join(dataDir, "cache", "galaxy", "home", ".ansible"),
		filepath.Join(dataDir, "cache", "galaxy", "tmp"),
	} {
		if fi, statErr := os.Stat(d); statErr != nil || !fi.IsDir() {
			t.Errorf("galaxy cache dir %s not pre-created: %v", d, statErr)
		}
	}
}

// TestResolveSourceCollectionsSandboxReadsLocalCollection is the kernel-level
// proof behind TestResolveSourceCollectionsSandboxGrantsLocalCollection: under
// a real nono, the confined ansible-galaxy can open the galaxy.yml of a
// type: dir collection named outside the project root. Without the read grant
// the open is refused with EACCES and the provision fails — the exact live bug
// this fix closes. A fake launcher cannot reproduce a refusal only the kernel
// makes, so a real nono is required.
func TestResolveSourceCollectionsSandboxReadsLocalCollection(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real nono sandbox")
	}
	if _, err := exec.LookPath(executor.NonoBinary); err != nil {
		t.Skipf("%s not on PATH", executor.NonoBinary)
	}
	resetSandboxDecision(t)
	sandbox.requested = executor.TierEnforcing
	sandbox.result = executor.ProbeResult{Requested: executor.TierEnforcing, Effective: executor.TierEnforcing, ProxyEnforced: true}
	sandbox.once.Do(func() {})

	project := t.TempDir()
	local := filepath.Join(t.TempDir(), "unifi_network")
	if err := os.MkdirAll(local, 0o700); err != nil {
		t.Fatalf("mkdir local collection: %v", err)
	}
	writeFile(t, filepath.Join(local, "galaxy.yml"), "namespace: abn\nname: unifi_network\n")

	binDir := t.TempDir()
	galaxy := filepath.Join(binDir, "ansible-galaxy")
	script := `#!/bin/sh
cat '` + filepath.Join(local, "galaxy.yml") + `' > /dev/null || exit 1
for a in "$@"; do target="$a"; done
mkdir -p "$target/ansible_collections/abn/unifi_network" || exit 1
exit 0
`
	if err := os.WriteFile(galaxy, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ansible-galaxy: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	writeFile(t, filepath.Join(project, "requirements.yml"),
		"collections:\n  - name: "+local+"\n    type: dir\n")
	play := filepath.Join(project, "site.yml")
	dataDir := t.TempDir()

	c, err := resolveSourceCollections(context.Background(), dataDir, play)
	if err != nil {
		t.Fatalf("resolve under the enforcing tier failed: %v", err)
	}
	if !c.Created {
		t.Fatal("expected a fresh collections provision through the resolver sandbox")
	}
	if _, statErr := os.Stat(filepath.Join(c.Dir, "ansible_collections", "abn", "unifi_network")); statErr != nil {
		t.Fatalf("local collection was not installed: %v", statErr)
	}
}

// TestAnsibleVenvAnchor: a playbook run discovers its manifest beside the
// playbook, an ad-hoc run from a source discovers it in the source checkout, and
// an ad-hoc run with no source stays on the image toolchain.
func TestAnsibleVenvAnchor(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "pyproject.toml"),
		[]byte("[project]\nname = \"x\"\nversion = \"0\"\ndependencies = [\"ansible-core\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adhoc := executor.AnsibleAdhocArgv("localhost", "", "ping", "")
	play := executor.AnsibleArgv("playbooks/site.yml", "")

	if got := ansibleVenvAnchor(play, src, "homelab"); got != ansiblePlaybookPath(play) {
		t.Errorf("playbook anchor = %q, want the playbook path %q", got, ansiblePlaybookPath(play))
	}
	if got := ansibleVenvAnchor(adhoc, src, ""); got != "" {
		t.Errorf("sourceless ad-hoc anchor = %q, want empty", got)
	}
	if got := ansibleVenvAnchor(adhoc, "", "homelab"); got != "" {
		t.Errorf("dirless ad-hoc anchor = %q, want empty", got)
	}
	anchor := ansibleVenvAnchor(adhoc, src, "homelab")
	if filepath.Dir(anchor) != src {
		t.Fatalf("ad-hoc anchor %q is not in the source checkout %q", anchor, src)
	}
	m, err := deps.DiscoverPython(filepath.Dir(anchor))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !m.HasDeps() || m.Root != src {
		t.Errorf("manifest = %+v, want the source's pyproject with deps", m)
	}
}

// fakeVenvUV writes a uv stand-in whose `venv` subcommand lays down the bin/python
// and site-packages a real venv carries, plus an ansible-playbook and an ansible
// that records its environment to probe.
func fakeVenvUV(t *testing.T, bin, probe string) {
	t.Helper()
	script := `#!/bin/sh
case "$*" in
  *venv*)
    for a in "$@"; do target="$a"; done
    mkdir -p "$target/bin" "$target/lib/python3.14/site-packages" || exit 1
    : > "$target/bin/python"; chmod +x "$target/bin/python"
    printf '#!/bin/sh\nexit 0\n' > "$target/bin/ansible-playbook"; chmod +x "$target/bin/ansible-playbook"
    printf '#!/bin/sh\nenv > ` + probe + `\nexit 0\n' > "$target/bin/ansible"; chmod +x "$target/bin/ansible"
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake uv: %v", err)
	}
}

// TestAnsibleDriverAdhocRunsInSourceVenv proves an ad-hoc run from a source that
// declares dependencies executes the venv's ansible, with no ansible on PATH.
func TestAnsibleDriverAdhocRunsInSourceVenv(t *testing.T) {
	forceDisabledTier(t)
	dir := initForSource(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	probe := filepath.Join(t.TempDir(), "child.env")
	binDir := t.TempDir()
	fakeVenvUV(t, binDir, probe)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	if p, err := exec.LookPath("ansible"); err == nil {
		t.Skipf("ansible on PATH at %s; the test needs it absent", p)
	}

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "requirements.txt"), "ansible-core\n")

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	q := queue.New(st, ansibleJobDriver{store: st, dataDir: dir}, queue.Options{Workers: 1, PollInterval: 20 * time.Millisecond})
	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook: "ansible",
		Kind:    executor.KindAnsible,
		Trigger: "manual",
		Argv:    executor.AnsibleAdhocArgv("localhost", "", "ping", ""),
		Dir:     src,
		Source:  "demo",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := q.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, err := os.Stat(probe); err != nil {
		t.Fatalf("the venv's ansible did not run (job %d):\n%s", job.ID, jobEventText(t, job.ID))
	}
}
