//go:build linux

package executor

import "golang.org/x/sys/unix"

// landlockABI asks the kernel which Landlock ABI it implements. Passing a nil
// attribute with the version flag is the documented probe: it creates no
// ruleset and returns the ABI number. ABI 4 is where TCP network rules arrive
// (kernel 6.7), which is what the per-kind egress ports need.
func landlockABI() (int, error) {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0,
		unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, errno
	}
	return int(v), nil
}

// LandlockNetABI is the ABI at which Landlock can filter TCP connect by port.
// Below it nono falls back to a seccomp baseline, which is coarser.
const LandlockNetABI = 4
