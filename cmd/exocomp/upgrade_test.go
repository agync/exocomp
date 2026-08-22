package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/store"
)

// writeFakeExo writes a stand-in exocomp binary: a shell script answering
// `version` (printing ver) and `doctor`. doctor exits non-zero when a
// HEALTH_FAIL marker exists in the data dir it is pointed at — the hook a test
// uses to make a binary pass verify (run against a clean scratch dir) but fail
// the post-swap health check (run against the live dir).
func writeFakeExo(t *testing.T, path, ver string) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  version) printf '%s\\n' '" + ver + "' ;;\n" +
		"  doctor) [ -f \"$XDG_DATA_HOME/exocomp/HEALTH_FAIL\" ] && exit 1 ; exit 0 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake exo %s: %v", path, err)
	}
}

// writeBrokenExo writes a stand-in that fails `doctor` unconditionally, so an
// upgrade to it fails at verify.
func writeBrokenExo(t *testing.T, path string) {
	t.Helper()
	body := "#!/bin/sh\ncase \"$1\" in\n  version) echo 0.0.0 ;;\n  doctor) exit 1 ;;\n  *) exit 0 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write broken exo %s: %v", path, err)
	}
}

// upgradeFixture sets up a scratch data dir plus a writable bin dir holding a
// live stand-in binary, and returns the live path.
func upgradeFixture(t *testing.T, liveVer string) (data, live string) {
	t.Helper()
	root := t.TempDir()
	data = filepath.Join(root, "data")
	useDataDir(t, data)
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	live = filepath.Join(binDir, "exocomp")
	writeFakeExo(t, live, liveVer)
	return data, live
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestUpgradeGoodSwapsBinary: an upgrade to a verifying binary runs all five
// steps to succeeded and leaves the new binary live.
//
// Mutation check: making swap a no-op (skip the rename) leaves live == old
// bytes and fails the final comparison; making the workflow report success
// without running trips the "5 succeeded steps" ledger assertion.
func TestUpgradeGoodSwapsBinary(t *testing.T) {
	_, live := upgradeFixture(t, "1.0.0")
	old := readBytes(t, live)

	newBin := filepath.Join(t.TempDir(), "new")
	writeFakeExo(t, newBin, "2.0.0")
	want := readBytes(t, newBin)

	var out bytes.Buffer
	if err := upgradeWithBinary(context.Background(), &out, live, newBin); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out.String())
	}

	for _, kind := range []string{"upgrade.snapshot", "upgrade.stage", "upgrade.verify", "upgrade.swap", "upgrade.health"} {
		if !strings.Contains(out.String(), kind) {
			t.Errorf("ledger did not list %s:\n%s", kind, out.String())
		}
	}
	if strings.Count(out.String(), "succeeded") < 5 {
		t.Errorf("want 5 succeeded steps:\n%s", out.String())
	}
	if got := readBytes(t, live); !bytes.Equal(got, want) {
		t.Fatalf("live binary was not swapped: %d bytes, want the new %d", len(got), len(want))
	}
	if bytes.Equal(readBytes(t, live), old) {
		t.Fatal("live binary still holds the old bytes")
	}
}

// TestUpgradeBrokenVerifyRollsBack: an upgrade to a binary that fails doctor
// stops at verify, blocks the rest, and never touches the live binary.
func TestUpgradeBrokenVerifyRollsBack(t *testing.T) {
	_, live := upgradeFixture(t, "1.0.0")
	old := readBytes(t, live)

	broken := filepath.Join(t.TempDir(), "broken")
	writeBrokenExo(t, broken)

	var out bytes.Buffer
	err := upgradeWithBinary(context.Background(), &out, live, broken)
	if err == nil {
		t.Fatalf("upgrade to a broken binary should fail:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "upgrade.verify") || !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("error should name the failed verify step and the rollback: %v", err)
	}
	if !bytes.Equal(readBytes(t, live), old) {
		t.Fatal("a verify failure must never have touched the live binary")
	}
	// The staged binary must not be left lying beside the live one.
	stray, _ := filepath.Glob(filepath.Join(filepath.Dir(live), stagedBinPre+"*"))
	if len(stray) != 0 {
		t.Errorf("staged binary left behind after rollback: %v", stray)
	}
}

// TestUpgradeHealthFailureRestoresBinary is the swap-then-rollback case: the new
// binary verifies (clean scratch dir) but fails the post-swap health check (the
// live dir carries a HEALTH_FAIL marker), so the swap DID happen and the
// rollback must put the old binary and data snapshot back.
//
// Mutation check: making restorePriorState skip the binary restore leaves live
// == new bytes and fails the "restored to old" assertion.
func TestUpgradeHealthFailureRestoresBinary(t *testing.T) {
	data, live := upgradeFixture(t, "1.0.0")
	old := readBytes(t, live)

	// The marker makes the swapped binary's doctor fail, but only against the
	// live data dir — verify runs against a scratch dir and passes.
	if err := os.MkdirAll(filepath.Join(data, "exocomp"), 0o700); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(data, "exocomp", "HEALTH_FAIL"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	newBin := filepath.Join(t.TempDir(), "new")
	writeFakeExo(t, newBin, "2.0.0")

	var out bytes.Buffer
	err := upgradeWithBinary(context.Background(), &out, live, newBin)
	if err == nil {
		t.Fatalf("upgrade whose health check fails should fail:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "upgrade.health") {
		t.Errorf("error should name the failed health step: %v", err)
	}
	if !strings.Contains(out.String(), "restored prior binary") {
		t.Errorf("rollback should have restored the prior binary:\n%s", out.String())
	}
	if !bytes.Equal(readBytes(t, live), old) {
		t.Fatal("health failure after swap must restore the old binary")
	}
}

// TestUpgradeRefusesUnwritableBinDir: when the binary's directory is not
// writable, upgrade fails clearly with the manual step and never escalates.
func TestUpgradeRefusesUnwritableBinDir(t *testing.T) {
	_, live := upgradeFixture(t, "1.0.0")
	binDir := filepath.Dir(live)
	if err := os.Chmod(binDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(binDir, 0o700) })

	newBin := filepath.Join(t.TempDir(), "new")
	writeFakeExo(t, newBin, "2.0.0")

	var out bytes.Buffer
	err := upgradeWithBinary(context.Background(), &out, live, newBin)
	if err == nil {
		t.Fatal("upgrade into an unwritable dir should fail")
	}
	for _, want := range []string{"not writable", "install -m0755", "never escalates"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// TestRollbackRestoresMostRecentUpgrade: after a good upgrade, `rollback`
// restores the prior binary on demand.
func TestRollbackRestoresMostRecentUpgrade(t *testing.T) {
	_, live := upgradeFixture(t, "1.0.0")
	old := readBytes(t, live)

	newBin := filepath.Join(t.TempDir(), "new")
	writeFakeExo(t, newBin, "2.0.0")
	newBytes := readBytes(t, newBin)

	var out bytes.Buffer
	if err := upgradeWithBinary(context.Background(), &out, live, newBin); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out.String())
	}
	if !bytes.Equal(readBytes(t, live), newBytes) {
		t.Fatal("precondition: upgrade did not swap")
	}

	var rout bytes.Buffer
	if err := rollbackWithBinary(context.Background(), &rout, live); err != nil {
		t.Fatalf("rollback: %v\n%s", err, rout.String())
	}
	if !bytes.Equal(readBytes(t, live), old) {
		t.Fatalf("rollback did not restore the prior binary:\n%s", rout.String())
	}
}

// TestRollbackLeavesNoResumableWorkflow: after a rollback restores the data
// snapshot (which carries the upgrade's own in-flight ledger rows), the restored
// database must hold no incomplete workflow — otherwise a later boot sweep would
// try to resume upgrade steps through a driver that cannot run them.
//
// Mutation check: dropping the terminalizeWorkflow call leaves the restored
// workflow's steps pending/running, and ListIncompleteWorkflows returns it —
// failing the "no incomplete workflow" assertion.
func TestRollbackLeavesNoResumableWorkflow(t *testing.T) {
	ctx := context.Background()
	data, live := upgradeFixture(t, "1.0.0")

	newBin := filepath.Join(t.TempDir(), "new")
	writeFakeExo(t, newBin, "2.0.0")

	var out bytes.Buffer
	if err := upgradeWithBinary(ctx, &out, live, newBin); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out.String())
	}
	var rout bytes.Buffer
	if err := rollbackWithBinary(ctx, &rout, live); err != nil {
		t.Fatalf("rollback: %v\n%s", err, rout.String())
	}
	if !strings.Contains(rout.String(), "restored data snapshot") {
		t.Fatalf("precondition: rollback did not restore the data snapshot:\n%s", rout.String())
	}

	st, err := openStoreAt(filepath.Join(data, "exocomp"))
	if err != nil {
		t.Fatalf("open restored store: %v", err)
	}
	defer func() { _ = st.Close() }()
	ids, err := st.ListIncompleteWorkflows(ctx)
	if err != nil {
		t.Fatalf("list incomplete: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("restored database has resumable workflows %v; the rollback must retire them", ids)
	}
}

// TestRollbackWarnsAndSnapshotsBeforeRestore is the item-4 gate: rollback names
// what its restore discards and takes a safety snapshot of the CURRENT database
// first, so a rollback run by mistake is itself reversible. A marker job is
// written AFTER the upgrade — a row the pre-upgrade snapshot cannot hold. After
// rollback the live DB has lost it (the discard the warning promised), while the
// safety snapshot still carries it (the reversibility the snapshot promises).
//
// Mutation check: dropping the preRollbackSafetySnapshot call leaves no
// pre-rollback dir and no "safety:" line, and restoring the (absent) snapshot
// fails — tripping the snapshot assertions; dropping warnRollbackDiscards trips
// the warning-substring assertions.
func TestRollbackWarnsAndSnapshotsBeforeRestore(t *testing.T) {
	ctx := context.Background()
	data, live := upgradeFixture(t, "1.0.0")
	dir := filepath.Join(data, "exocomp")

	newBin := filepath.Join(t.TempDir(), "new")
	writeFakeExo(t, newBin, "2.0.0")

	var out bytes.Buffer
	if err := upgradeWithBinary(ctx, &out, live, newBin); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out.String())
	}

	// A durable row that exists only AFTER the upgrade: rollback's restore of the
	// pre-upgrade snapshot must discard it, and the safety snapshot must keep it.
	const marker = "post-upgrade-marker"
	writeMarkerJob(ctx, t, dir, marker)

	var rout bytes.Buffer
	if err := rollbackWithBinary(ctx, &rout, live); err != nil {
		t.Fatalf("rollback: %v\n%s", err, rout.String())
	}

	// The warning names the blast radius, by category.
	for _, want := range []string{"WARNING", "DISCARDED", "jobs", "backups", "workspaces", "secrets"} {
		if !strings.Contains(rout.String(), want) {
			t.Errorf("rollback warning missing %q:\n%s", want, rout.String())
		}
	}
	if !strings.Contains(rout.String(), "safety:") {
		t.Errorf("rollback did not report a safety snapshot:\n%s", rout.String())
	}

	// The live database lost the post-upgrade row (rollback discarded it).
	if hasMarkerJob(ctx, t, dir, marker) {
		t.Error("rollback did not discard the post-upgrade row from the live database")
	}

	// The safety snapshot captured the current database: restoring it into a
	// scratch data dir recovers the row the rollback threw away.
	snap := latestPreRollbackSnapshot(t, dir)
	scratch := t.TempDir()
	if err := backup.LocalRestore(snap, scratch); err != nil {
		t.Fatalf("restore safety snapshot: %v", err)
	}
	if !hasMarkerJob(ctx, t, scratch, marker) {
		t.Error("safety snapshot did not capture the current database (marker row absent) — " +
			"a mistaken rollback would not be reversible")
	}
}

// writeMarkerJob inserts a queued job under a distinctive idempotency key into
// the store at dir — a cheap durable row for the safety-snapshot test.
func writeMarkerJob(ctx context.Context, t *testing.T, dir, key string) {
	t.Helper()
	st, err := openStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.CreateJob(ctx, store.NewJob{
		Runbook: "marker", Kind: "shell", Trigger: "manual", IdempotencyKey: key,
	}); err != nil {
		t.Fatalf("create marker job: %v", err)
	}
}

// hasMarkerJob reports whether the store at dir holds the marker job.
func hasMarkerJob(ctx context.Context, t *testing.T, dir, key string) bool {
	t.Helper()
	st, err := openStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	_, err = st.JobByIdempotencyKey(ctx, key)
	if err == nil {
		return true
	}
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	t.Fatalf("look up marker job: %v", err)
	return false
}

// latestPreRollbackSnapshot returns the newest pre-rollback safety snapshot dir
// under the data dir, failing if none was written.
func latestPreRollbackSnapshot(t *testing.T, dir string) string {
	t.Helper()
	root := filepath.Join(dir, upgradeSubdir, preRollbackSubdir)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read pre-rollback snapshots under %s: %v", root, err)
	}
	best := ""
	for _, e := range entries {
		if e.IsDir() && e.Name() > best {
			best = e.Name()
		}
	}
	if best == "" {
		t.Fatalf("no pre-rollback safety snapshot under %s", root)
	}
	return filepath.Join(root, best)
}

// TestRollbackWithNoUpgradePoint: rollback with nothing to undo fails clearly.
func TestRollbackWithNoUpgradePoint(t *testing.T) {
	_, live := upgradeFixture(t, "1.0.0")
	var out bytes.Buffer
	err := rollbackWithBinary(context.Background(), &out, live)
	if err == nil || !strings.Contains(err.Error(), "no upgrade rollback point") {
		t.Fatalf("want a clear no-rollback-point error, got %v", err)
	}
}
