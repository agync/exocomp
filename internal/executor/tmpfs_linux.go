//go:build linux

package executor

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// runDirFSType returns the statfs f_type of the filesystem backing dir. It is
// the primitive the tmpfs assertion is built on: a per-job private root that is
// not a RAM-only filesystem is durable-disk storage for whatever is staged
// there, which the sealed-bearer file path (Phase 3) refuses.
func runDirFSType(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("executor: statfs %s: %w", dir, err)
	}
	return int64(st.Type), nil
}

// tmpfsMagic is the statfs f_type reported for a tmpfs mount.
const tmpfsMagic = int64(unix.TMPFS_MAGIC)
