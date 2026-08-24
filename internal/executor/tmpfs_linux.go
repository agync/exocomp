//go:build linux

package executor

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// statfsProbe reports the filesystem type and free bytes available to an
// unprivileged writer for dir, the raw statfs(2) answer the tmpfs usability
// check builds on.
func statfsProbe(dir string) (fsType int64, freeBytes uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, fmt.Errorf("executor: statfs %s: %w", dir, err)
	}
	// Bavail, not Bfree: free space this process could actually allocate,
	// excluding blocks reserved for a privileged writer it is not.
	return int64(st.Type), uint64(st.Bavail) * uint64(st.Bsize), nil
}

// tmpfsMagic is the statfs f_type reported for a tmpfs mount.
const tmpfsMagic = int64(unix.TMPFS_MAGIC)
