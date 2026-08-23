//go:build linux

package executor

import "syscall"

// applyCgroupFD tells the kernel to create the child directly inside the cgroup
// named by fd, via clone3's CLONE_INTO_CGROUP. The process therefore starts life
// already capped — it is never briefly a member of the daemon's own cgroup where
// the memory ceiling would not yet apply. Setpgid still holds; the two combine.
func applyCgroupFD(attr *syscall.SysProcAttr, fd int) {
	attr.UseCgroupFD = true
	attr.CgroupFD = fd
}
