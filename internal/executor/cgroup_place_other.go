//go:build !linux

package executor

import "syscall"

// applyCgroupFD is a no-op off Linux: CLONE_INTO_CGROUP is a Linux clone3
// feature, and per-job caps are a Linux-container capability either way. The
// controller never prepares off Linux (selfCgroupDir has no /proc to read), so
// this is unreachable in practice and exists only to keep the build cross-
// platform.
func applyCgroupFD(_ *syscall.SysProcAttr, _ int) {}
