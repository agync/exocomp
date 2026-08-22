package litestream

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplicaFreshnessFileReplica(t *testing.T) {
	dir := t.TempDir()
	// Litestream lays WAL frames as ordinary files under the replica path; the
	// newest mtime is the last replicated frame.
	walDir := filepath.Join(dir, "generations", "abc123", "wal")
	if err := os.MkdirAll(walDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	frame := filepath.Join(walDir, "0000000000000001.wal.lz4")
	if err := os.WriteFile(frame, []byte("frame"), 0o644); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	mtime := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	if err := os.Chtimes(frame, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	now := mtime.Add(30 * time.Second)

	age, got, ok, err := ReplicaFreshness(Target{Type: ReplicaFile, Path: dir}, now)
	if err != nil {
		t.Fatalf("freshness: %v", err)
	}
	if !ok {
		t.Fatal("want ok=true for a file replica holding a frame")
	}
	if age != 30*time.Second {
		t.Errorf("age = %v, want 30s", age)
	}
	if !got.Equal(mtime) {
		t.Errorf("mtime = %v, want %v", got, mtime)
	}
}

func TestReplicaFreshnessNoFramesYet(t *testing.T) {
	// An empty replica dir, and a path that does not exist, both read as
	// "unknown" (ok=false) with no error — no frames is a state, not a fault.
	empty := t.TempDir()
	if _, _, ok, err := ReplicaFreshness(Target{Type: ReplicaFile, Path: empty}, time.Now()); ok || err != nil {
		t.Errorf("empty dir: ok=%v err=%v, want false/nil", ok, err)
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, _, ok, err := ReplicaFreshness(Target{Type: ReplicaFile, Path: missing}, time.Now()); ok || err != nil {
		t.Errorf("missing dir: ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestReplicaFreshnessRemoteUnknown(t *testing.T) {
	// A remote target's frame age can only be read by litestream itself, so
	// exocomp reports it as unknown rather than guessing.
	if _, _, ok, err := ReplicaFreshness(Target{Type: ReplicaS3, Bucket: "b", Path: "p"}, time.Now()); ok || err != nil {
		t.Errorf("s3 target: ok=%v err=%v, want false/nil", ok, err)
	}
}
