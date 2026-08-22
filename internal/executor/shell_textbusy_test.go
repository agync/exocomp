package executor_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// TestShellStartRetriesTextBusy proves the driver does not surface the ETXTBSY
// fork/exec race: many goroutines each write a fresh executable and run it
// through the Shell driver at once, so a sibling's fork+exec keeps capturing the
// just-written writable descriptor. Without the bounded start retry some Run
// calls fail with "text file busy"; with it every run reaches its exit code.
//
// This is the regression for the flaky ansible fidelity test — every driver
// funnels its subprocess through Shell.Run, so hardening the start there covers
// the whole executor surface, not one fake tool.
func TestShellStartRetriesTextBusy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const workers = 256

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			bin := filepath.Join(dir, fmt.Sprintf("tool-%d", n))
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				errs <- err
				return
			}
			res, err := executor.NewShell().Run(context.Background(),
				executor.Spec{Argv: []string{bin}}, &collector{})
			if err != nil {
				errs <- fmt.Errorf("worker %d: %w", n, err)
				return
			}
			if res.ExitCode != 0 {
				errs <- fmt.Errorf("worker %d: exit code %d", n, res.ExitCode)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	var busy, other int
	for err := range errs {
		if errors.Is(err, syscall.ETXTBSY) {
			busy++
		} else {
			other++
			t.Errorf("unexpected run failure: %v", err)
		}
	}
	if busy > 0 {
		t.Fatalf("%d run(s) failed with ETXTBSY: the start retry did not absorb the fork/exec race", busy)
	}
}
