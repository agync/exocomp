package main

import (
	"context"
	"os"
	"path/filepath"
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

// TestResolveSourceCollectionsProvisionsPerSourceDir is the ADR 0015 resolve
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
// ansiblePlaybookPath returns "" and collections resolve skips it — matching
// the venv resolve — with no ansible-galaxy invocation.
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
// source resolved collections, the shared dir alone otherwise (ADR 0015).
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
// the shared fallback (ADR 0015), and that a second run of an unchanged source
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
