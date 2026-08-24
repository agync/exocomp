package executor

import (
	"os"
	"os/exec"
	"testing"
)

// TestPidAliveOwnProcess proves a process is alive to its own liveness probe —
// the case that must hold for a running daemon never to sweep its own,
// still-open staging session directory.
func TestPidAliveOwnProcess(t *testing.T) {
	t.Parallel()
	if !pidAlive(os.Getpid()) {
		t.Fatal("the current process reads as dead to its own pid")
	}
}

// TestPidAliveDeadProcess proves a pid that has already exited reads as dead:
// the liveness half the sweep's mutation test needs a real, verified case for.
func TestPidAliveDeadProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	pid := cmd.Process.Pid
	if pidAlive(pid) {
		t.Fatalf("exited pid %d still reads as alive", pid)
	}
}

// TestPidAliveRejectsNonPositive proves a terminal or never-owned value (<= 0)
// is never treated as alive, matching store.ownerAlive's own guard.
func TestPidAliveRejectsNonPositive(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{0, -1} {
		if pidAlive(pid) {
			t.Errorf("pidAlive(%d) = true, want false", pid)
		}
	}
}
