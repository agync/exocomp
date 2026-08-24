package executor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveStagingAutoUsesTmpfsWhenUsable proves the default (auto) backend
// resolves to the real tmpfs root when it is usable, with no fallback.
func TestResolveStagingAutoUsesTmpfsWhenUsable(t *testing.T) {
	t.Parallel()
	st, err := ResolveStaging(StagingConfig{Backend: StagingAuto})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if st.Backend != StagingTmpfs {
		t.Errorf("backend = %s, want %s", st.Backend, StagingTmpfs)
	}
	if st.Root != TmpfsRoot {
		t.Errorf("root = %s, want %s", st.Root, TmpfsRoot)
	}
	if st.FellBack {
		t.Error("auto fell back to disk even though tmpfs is usable")
	}
	if !st.TmpfsUsable {
		t.Error("TmpfsUsable = false even though tmpfs was selected")
	}
}

// TestResolveStagingAutoFallsBackToDiskLoudly is Change 2's central case:
// auto asked for tmpfs, tmpfs is unusable (simulated by pointing the probe at
// a non-tmpfs directory), and the result is a LOUD disk fallback — resolved to
// the disk root, FellBack set, and a warning actually written to the warn
// writer, never a silent downgrade (ADR 0005).
func TestResolveStagingAutoFallsBackToDiskLoudly(t *testing.T) {
	t.Parallel()
	diskRoot := t.TempDir()
	var warn bytes.Buffer

	st, err := ResolveStaging(StagingConfig{
		Backend:           StagingAuto,
		TmpfsRootOverride: nonTmpfsDir(t),
		DiskRoot:          diskRoot,
		Warn:              &warn,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if st.Backend != StagingDisk {
		t.Errorf("backend = %s, want %s", st.Backend, StagingDisk)
	}
	if st.Root != diskRoot {
		t.Errorf("root = %s, want %s", st.Root, diskRoot)
	}
	if !st.FellBack {
		t.Error("FellBack = false for an auto fallback")
	}
	if st.TmpfsUsable {
		t.Error("TmpfsUsable = true for the unusable probe target")
	}
	if warn.Len() == 0 {
		t.Fatal("auto's fallback to disk wrote nothing to the warn writer — a silent downgrade")
	}
	if !strings.Contains(warn.String(), "WARNING") || !strings.Contains(warn.String(), diskRoot) {
		t.Errorf("fallback warning does not read as loud and specific: %q", warn.String())
	}
}

// TestResolveStagingAutoWithoutDiskRootRefuses proves auto cannot silently
// stage nowhere: with tmpfs unusable and no disk root configured to fall back
// to, resolution refuses rather than returning something that would later
// fail confusingly at the first actual stage.
func TestResolveStagingAutoWithoutDiskRootRefuses(t *testing.T) {
	t.Parallel()
	_, err := ResolveStaging(StagingConfig{
		Backend:           StagingAuto,
		TmpfsRootOverride: nonTmpfsDir(t),
	})
	if err == nil {
		t.Fatal("auto with an unusable tmpfs and no disk root did not refuse")
	}
}

// TestResolveStagingTmpfsFailsClosedWhenUnusable is the strict backend's
// contract: tmpfs asked for by name refuses outright when unusable, rather
// than falling back to disk the way auto does — "never a secret on disk" is a
// policy an operator can force, not merely a default that yields.
func TestResolveStagingTmpfsFailsClosedWhenUnusable(t *testing.T) {
	t.Parallel()
	_, err := ResolveStaging(StagingConfig{
		Backend:           StagingTmpfs,
		TmpfsRootOverride: nonTmpfsDir(t),
		DiskRoot:          t.TempDir(), // present but must be ignored: tmpfs never falls back.
	})
	if err == nil {
		t.Fatal("an explicit tmpfs backend accepted an unusable tmpfs root")
	}
}

// TestResolveStagingTmpfsSucceedsWhenUsable is TestResolveStagingTmpfsFailsClosedWhenUnusable's
// positive twin: the same explicit backend against the real, usable tmpfs
// root resolves cleanly.
func TestResolveStagingTmpfsSucceedsWhenUsable(t *testing.T) {
	t.Parallel()
	st, err := ResolveStaging(StagingConfig{Backend: StagingTmpfs})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if st.Backend != StagingTmpfs || st.Root != TmpfsRoot {
		t.Errorf("resolved %s at %s, want %s at %s", st.Backend, st.Root, StagingTmpfs, TmpfsRoot)
	}
}

// TestResolveStagingDiskUsesDiskRootExplicitly proves an explicit disk
// backend uses the disk root even though tmpfs is usable in this
// environment — it is a deliberate operator choice, not a fallback, so
// FellBack must read false.
func TestResolveStagingDiskUsesDiskRootExplicitly(t *testing.T) {
	t.Parallel()
	diskRoot := t.TempDir()
	st, err := ResolveStaging(StagingConfig{Backend: StagingDisk, DiskRoot: diskRoot})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if st.Backend != StagingDisk || st.Root != diskRoot {
		t.Errorf("resolved %s at %s, want %s at %s", st.Backend, st.Root, StagingDisk, diskRoot)
	}
	if st.FellBack {
		t.Error("an explicit disk backend read as a fallback")
	}
}

// TestResolveStagingDiskRequiresRoot proves disk without a configured root
// refuses rather than resolving to an empty path.
func TestResolveStagingDiskRequiresRoot(t *testing.T) {
	t.Parallel()
	if _, err := ResolveStaging(StagingConfig{Backend: StagingDisk}); err == nil {
		t.Fatal("disk backend with no configured root did not refuse")
	}
}

// TestParseStagingBackendRoundTrips proves every accepted spelling parses back
// to itself, empty means auto, and an unknown name is refused.
func TestParseStagingBackendRoundTrips(t *testing.T) {
	t.Parallel()
	for _, b := range []StagingBackend{StagingAuto, StagingTmpfs, StagingDisk} {
		got, err := ParseStagingBackend(string(b))
		if err != nil || got != b {
			t.Errorf("ParseStagingBackend(%q) = %q, %v; want %q, nil", b, got, err, b)
		}
	}
	if got, err := ParseStagingBackend(""); err != nil || got != StagingAuto {
		t.Errorf("ParseStagingBackend(\"\") = %q, %v; want %q, nil", got, err, StagingAuto)
	}
	if _, err := ParseStagingBackend("paranoid"); err == nil {
		t.Error("an unknown backend name was accepted")
	}
}

// TestStagingStageFilesOnDiskBackend is the disk-residual-bounded test:
// staging onto an explicit disk backend produces a 0600 file inside a 0700
// session directory, best-effort mlock is attempted (Lock.Err nil or a
// documented reason, never left unset), Close unlinks the individual file, and
// Staging.Close then removes the whole session directory — nothing left
// behind on durable storage once the instance shuts down cleanly.
func TestStagingStageFilesOnDiskBackend(t *testing.T) {
	t.Parallel()
	diskRoot := t.TempDir()
	st, err := ResolveStaging(StagingConfig{Backend: StagingDisk, DiskRoot: diskRoot})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rs := RunSecrets{"KUBECONFIG": "disk-backend-sentinel\n"}
	out, staged, err := st.StageFiles(rs, []string{"KUBECONFIG"})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(staged) != 1 {
		t.Fatalf("staged = %d files, want 1", len(staged))
	}
	path := staged[0].Path
	if out["KUBECONFIG"] != path {
		t.Errorf("rewritten env %q does not match staged path %q", out["KUBECONFIG"], path)
	}
	if staged[0].Backend != StagingDisk {
		t.Errorf("staged file backend = %s, want %s", staged[0].Backend, StagingDisk)
	}
	if !strings.HasPrefix(path, diskRoot+"/") {
		t.Fatalf("staged path %q is not under the disk root %q", path, diskRoot)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("staged file mode = %o, want 600", info.Mode().Perm())
	}
	sessionDir := st.SessionDir()
	if sessionDir == "" || filepath.Dir(path) != sessionDir {
		t.Fatalf("staged path %q is not inside the reported session dir %q", path, sessionDir)
	}
	dirInfo, err := os.Stat(sessionDir)
	if err != nil {
		t.Fatalf("stat session dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("session dir mode = %o, want 700", dirInfo.Mode().Perm())
	}
	// mlock is best-effort: Lock.Err is nil (locked) or carries the reason it
	// could not be (RLIMIT_MEMLOCK etc) — either is a valid, reported
	// outcome; the only failure is the field silently unset, which a zero
	// MemoryLock (Locked: false, Err: nil) would be.
	if !staged[0].Lock.Locked && staged[0].Lock.Err == nil {
		t.Error("mlock reported neither locked nor a reason it was not")
	}

	if err := staged[0].Close(); err != nil {
		t.Fatalf("close staged file: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("staged file survived its own Close: stat err = %v", err)
	}
	// The session dir itself survives a per-file Close — only the whole
	// instance shutting down removes it.
	if _, err := os.Stat(sessionDir); err != nil {
		t.Errorf("session dir was removed by a per-file Close: %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("staging close: %v", err)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("session dir survived Staging.Close: stat err = %v", err)
	}
	if st.SessionDir() != "" {
		t.Errorf("SessionDir() = %q after Close, want empty", st.SessionDir())
	}
}

// TestStagingCloseIsIdempotent proves a second Close on an already-closed
// instance (nothing left to remove) is a no-op, not an error — the shape
// cmd/exocomp's shutdown defer needs, since it always calls it exactly once
// but a test exercising the same instance twice must not see a spurious
// failure either.
func TestStagingCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	st, err := ResolveStaging(StagingConfig{Backend: StagingDisk, DiskRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close on a never-staged instance: %v", err)
	}
	if _, _, err := st.StageFiles(RunSecrets{"K": "v"}, []string{"K"}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestNoteDiskStagingResidualOmitsSecretValue pins that the disk-residual
// LevelSystem job event noteDiskStagingResidual builds never carries the
// staged secret's own bytes — only the env name and a fixed, secret-free
// description. The tmpfs path's no-secret-in-output property already has
// end-to-end coverage (TestTofuFileStagedSecretReachesChildAsPath); this is
// the disk-backend counterpart, pinning the event this package itself
// constructs rather than relying on it never being exercised.
func TestNoteDiskStagingResidualOmitsSecretValue(t *testing.T) {
	t.Parallel()
	diskRoot := t.TempDir()
	st, err := ResolveStaging(StagingConfig{Backend: StagingDisk, DiskRoot: diskRoot})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	const sentinel = "s3cr3t-disk-residual-bearer"
	_, staged, err := st.StageFiles(RunSecrets{"KUBECONFIG": sentinel}, []string{"KUBECONFIG"})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	t.Cleanup(func() {
		for i := range staged {
			_ = staged[i].Close()
		}
	})

	var events []Event
	noteDiskStagingResidual(SinkFunc(func(e Event) { events = append(events, e) }), staged)

	if len(events) != 1 {
		t.Fatalf("noteDiskStagingResidual emitted %d events, want 1", len(events))
	}
	e := events[0]
	if e.Level != LevelSystem {
		t.Errorf("event level = %q, want %s", e.Level, LevelSystem)
	}
	if !strings.Contains(e.Msg, "staged to disk") {
		t.Errorf("event does not name the disk residual: %q", e.Msg)
	}
	if strings.Contains(e.Msg, sentinel) || strings.Contains(e.RawJSON, sentinel) {
		t.Errorf("the disk-residual event carried the secret's own bytes: %+v", e)
	}
}

// TestConfigureRunSecretStagingOverridesTheFreeFunctionDefault proves
// ConfigureRunSecretStaging is what StageRunSecretFiles' free-function form
// actually consults: configuring the disk backend makes the very next
// StageRunSecretFiles call land there, not on the tmpfs default. It mutates
// package-global state (activeStaging), so it saves and restores it rather
// than running in parallel with anything else in this package that relies on
// the lazy tmpfs-only default.
func TestConfigureRunSecretStagingOverridesTheFreeFunctionDefault(t *testing.T) {
	old := activeStaging.Load()
	t.Cleanup(func() { activeStaging.Store(old) })

	diskRoot := t.TempDir()
	st, err := ConfigureRunSecretStaging(StagingConfig{Backend: StagingDisk, DiskRoot: diskRoot})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if st.Backend != StagingDisk {
		t.Fatalf("configured backend = %s, want %s", st.Backend, StagingDisk)
	}

	_, staged, err := StageRunSecretFiles(RunSecrets{"K": "v"}, []string{"K"})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	t.Cleanup(func() { _ = staged[0].Close() })
	if !strings.HasPrefix(staged[0].Path, diskRoot+"/") {
		t.Errorf("StageRunSecretFiles staged %q, want it under the configured disk root %q",
			staged[0].Path, diskRoot)
	}

	if err := CloseRunSecretStaging(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
