package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// maxLineBytes caps one scanned output line. Longer lines are reported as a
// scan error event rather than growing the buffer without bound.
const maxLineBytes = 1 << 20

// Shell is the M1 local driver: one subprocess per job, output scanned line
// by line into events, exit code returned as-is.
type Shell struct{}

// NewShell returns the local subprocess driver.
func NewShell() *Shell { return &Shell{} }

// Run executes spec.Argv and streams its output to sink.
func (*Shell) Run(ctx context.Context, spec Spec, sink EventSink) (Result, error) {
	if len(spec.Argv) == 0 {
		return Result{}, errors.New("executor: empty argv")
	}

	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("executor: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("executor: stderr pipe: %w", err)
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("executor: start %s: %w", spec.Argv[0], err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scan(stdout, LevelInfo, sink)
	}()
	go func() {
		defer wg.Done()
		scan(stderr, LevelError, sink)
	}()
	wg.Wait()

	waitErr := cmd.Wait()
	res := Result{Duration: time.Since(started)}

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		res.ExitCode = cmd.ProcessState.ExitCode()
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return Result{}, fmt.Errorf("executor: wait for %s: %w", spec.Argv[0], waitErr)
	}

	res.Signaled = res.ExitCode < 0
	res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	return res, nil
}

func scan(r io.Reader, level string, sink EventSink) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for scanner.Scan() {
		sink.Emit(Event{Ts: time.Now().UTC(), Level: level, Msg: scanner.Text()})
	}
	// A closed pipe is how a killed process ends; it is not an output error.
	if err := scanner.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		sink.Emit(Event{
			Ts:    time.Now().UTC(),
			Level: LevelSystem,
			Msg:   fmt.Sprintf("output scan stopped: %v", err),
		})
	}
}
