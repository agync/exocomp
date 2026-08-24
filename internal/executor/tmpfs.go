package executor

import (
	"fmt"
	"os"
)

// TmpfsRoot is the RAM-backed tmpfs root a run-secret file is staged under
// when the tmpfs backend is in effect: /dev/shm, present unconditionally in
// every rootless podman container. Unlike a `Tmpfs=` quadlet mount it needs no
// mount, no quadlet entry, and no capability, and it is what lets file staging
// work under the default local-sandboxed tier — a nono `--read-file` grant on
// the staged path is all a sandboxed child needs (see ReadFiles on
// SandboxOptions).
const TmpfsRoot = "/dev/shm"

// minTmpfsFreeBytes is the free-space floor tmpfsUsable requires before
// selecting the tmpfs root: generous headroom for what this path ever writes
// (a kubeconfig or similar credential file, not a bulk artifact), while still
// catching a /dev/shm an operator sized down to nothing.
const minTmpfsFreeBytes = 1 << 20 // 1 MiB

// tmpfsUsable reports whether dir is fit to stage a run-secret file onto:
// present, a directory, actually backed by tmpfs (statfs f_type ==
// TMPFS_MAGIC), and holding at least minFree bytes free. It is a SELECTION
// probe for the auto/tmpfs backend choice — never a fail-closed assertion on
// its own; ResolveStaging decides what an unusable answer means for the
// backend that was actually requested (auto falls back loudly, tmpfs refuses).
// minFree <= 0 uses minTmpfsFreeBytes.
func tmpfsUsable(dir string, minFree int64) (bool, string) {
	if minFree <= 0 {
		minFree = minTmpfsFreeBytes
	}
	info, err := os.Stat(dir)
	if err != nil {
		return false, err.Error()
	}
	if !info.IsDir() {
		return false, fmt.Sprintf("%s is not a directory", dir)
	}
	fsType, free, err := statfsProbe(dir)
	if err != nil {
		return false, err.Error()
	}
	if fsType != tmpfsMagic {
		return false, fmt.Sprintf("%s is not backed by tmpfs", dir)
	}
	if free < uint64(minFree) {
		return false, fmt.Sprintf("%s has only %d bytes free, want at least %d", dir, free, minFree)
	}
	return true, ""
}
