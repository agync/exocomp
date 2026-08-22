package litestream

import (
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Presence reports whether the litestream binary is installed and, if so, its
// version line. It never errors on absence — a missing sidecar binary is the
// default state on a fresh host, not a fault. version is empty when the binary
// is absent or its version probe fails.
func Presence() (path, version string) {
	p, err := exec.LookPath(Binary)
	if err != nil {
		return "", ""
	}
	out, err := exec.Command(p, "version").CombinedOutput()
	if err != nil {
		return p, ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return p, strings.TrimSpace(line)
}

// ReplicaFreshness observes the age of the most recently replicated WAL frame
// by reading the replica's own layout — no litestream binary required for a
// file replica, whose frames are ordinary files exocomp can stat. It is the
// input to the RPO-age staleness gauge.
//
// ok is false, with a nil error, when freshness cannot be observed from
// exocomp's side: a remote (s3/sftp) target, whose frame age only litestream
// itself can read, or a file replica that holds no frames yet. That is reported
// honestly as "unknown" rather than as a misleading zero.
func ReplicaFreshness(t Target, now time.Time) (age time.Duration, mtime time.Time, ok bool, err error) {
	if t.Type != ReplicaFile || t.Path == "" {
		return 0, time.Time{}, false, nil
	}
	var newest time.Time
	found := false
	walkErr := filepath.WalkDir(t.Path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// A replica directory that does not exist yet is "no frames", not a
			// fault; skip a path we cannot descend and keep the newest we found.
			if d == nil {
				return fs.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if m := info.ModTime(); m.After(newest) {
			newest = m
			found = true
		}
		return nil
	})
	if walkErr != nil {
		return 0, time.Time{}, false, walkErr
	}
	if !found {
		return 0, time.Time{}, false, nil
	}
	d := now.Sub(newest)
	if d < 0 {
		d = 0
	}
	return d, newest, true, nil
}
