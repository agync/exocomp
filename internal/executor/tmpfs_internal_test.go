package executor

import (
	"os"
	"testing"
)

// nonTmpfsDir returns a directory guaranteed not to be tmpfs: a scratch
// subdirectory of the package's own source checkout, rather than t.TempDir()
// (which follows $TMPDIR/the OS default, itself tmpfs-backed in a container
// dev box like this repo's own — a real, if unusual, environment where
// t.TempDir() would make this test vacuously pass). Skips rather than fails
// if even the checkout itself turns out to be tmpfs-backed.
func nonTmpfsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "tmpfs-probe-test-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if ok, _ := tmpfsUsable(dir, 0); ok {
		t.Skip("this checkout's own filesystem is tmpfs-backed; no non-tmpfs directory available to test against")
	}
	return dir
}

// TestTmpfsUsableAcceptsRealTmpfs proves the probe accepts TmpfsRoot itself —
// the positive case every backend-selection test downstream relies on holding
// in this environment.
func TestTmpfsUsableAcceptsRealTmpfs(t *testing.T) {
	t.Parallel()
	ok, reason := tmpfsUsable(TmpfsRoot, 0)
	if !ok {
		t.Fatalf("%s did not read as usable tmpfs: %s", TmpfsRoot, reason)
	}
}

// TestTmpfsUsableRejectsNonTmpfsDir is the negative case backend selection's
// "tmpfs forces tmpfs and fails closed when unusable" test simulates against:
// a plain, non-tmpfs directory reads as not usable, with a reason naming why.
func TestTmpfsUsableRejectsNonTmpfsDir(t *testing.T) {
	t.Parallel()
	ok, reason := tmpfsUsable(nonTmpfsDir(t), 0)
	if ok {
		t.Fatal("a plain (non-tmpfs) directory read as usable tmpfs")
	}
	if reason == "" {
		t.Error("an unusable answer carries no reason")
	}
}

// TestTmpfsUsableRejectsMissingDir proves a nonexistent path is unusable
// rather than panicking or reading as usable by omission.
func TestTmpfsUsableRejectsMissingDir(t *testing.T) {
	t.Parallel()
	ok, reason := tmpfsUsable("/nonexistent/exocomp-tmpfs-probe-test", 0)
	if ok {
		t.Fatal("a nonexistent path read as usable tmpfs")
	}
	if reason == "" {
		t.Error("an unusable answer carries no reason")
	}
}

// TestTmpfsUsableRejectsInsufficientFreeSpace proves the free-space floor is
// enforced, not merely accepted: a real tmpfs asked to clear an absurd bar
// reads unusable rather than the check being skipped.
func TestTmpfsUsableRejectsInsufficientFreeSpace(t *testing.T) {
	t.Parallel()
	ok, reason := tmpfsUsable(TmpfsRoot, 1<<62)
	if ok {
		t.Fatal("an impossible free-space floor was reported as met")
	}
	if reason == "" {
		t.Error("an unusable answer carries no reason")
	}
}
