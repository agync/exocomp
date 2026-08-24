package executor

import (
	"errors"
	"syscall"
)

// pidAlive reports whether pid is still a running process, by the same
// existence-and-permission probe internal/store/job.go's ownerAlive uses:
// signal 0 performs the checks without sending anything, and EPERM means the
// process exists and belongs to someone else. It is duplicated here rather
// than imported — ownerAlive is unexported, and store.ownerAlive also folds in
// a same-pid/since comparison for job rows' own clock that a staging session
// directory (named only by pid, not by a start time) has no equivalent of.
//
// A pid this process itself holds always reads alive (a process can always
// signal itself), which is correct: an instance never sweeps its own,
// still-live session directory. The one gap this shares with ownerAlive is pid
// reuse — a container's pid 1 handed to a fresh process after a crash reads as
// "alive" for a directory a previous incarnation left behind. That is bounded
// the same way SweepOrphanRunSecretFiles' doc already notes: Restart=always
// clears /dev/shm on most crashes, and a disk-backed leftover is the
// documented residual, not a security exposure — the file inside it is still
// 0600, still unlinked per job, and gone once an operator restarts cleanly.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
