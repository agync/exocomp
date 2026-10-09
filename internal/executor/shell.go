package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxLineBytes caps one scanned output line. Longer lines are reported as a
// scan error event rather than growing the buffer without bound.
const maxLineBytes = 1 << 20

// pipeDrainGrace is how long the output pipes are read after the job's own
// process is gone. A grandchild that outlives its parent — a backgrounded
// helper, an ssh control master — inherits those descriptors and would
// otherwise hold the job open for as long as it runs.
const pipeDrainGrace = 2 * time.Second

// textBusyRetries bounds the ETXTBSY retry at start. ETXTBSY is a transient of
// exec'ing a file while a concurrent fork+exec elsewhere in this process still
// holds a writable descriptor to it (a vendored tool just written, or a fake
// tool in a parallel test): the kernel refuses the exec until that sibling
// child execs and its O_CLOEXEC descriptor closes — microseconds later. It is
// never a permanent condition, so a bounded backoff clears it deterministically
// rather than surfacing a spurious start failure.
const textBusyRetries = 12

// JobIDEnv is the environment marker every job subprocess carries, so a
// command a job runs (an exocomp CLI in particular) can tell it is executing
// inside a job rather than an operator's shell. Set from Spec.JobID, which the
// queue fills for every real job; a direct executor caller (a probe, a vendor
// step) leaves JobID zero and gets no marker.
const JobIDEnv = "EXOCOMP_JOB_ID"

// Shell is the local driver: one subprocess per job, output scanned line
// by line into events, exit code returned as-is.
type Shell struct{}

// NewShell returns the local subprocess driver.
func NewShell() *Shell { return &Shell{} }

// withJobContext returns env with the job marker set to jobID. A nil env means
// "inherit this process's environment" (exec.Cmd's zero-value meaning), so it
// is materialized from os.Environ first; any inherited marker is replaced so
// the value always names this job, never a parent job's.
func withJobContext(env []string, jobID int64) []string {
	base := env
	if base == nil {
		base = os.Environ()
	}
	prefix := JobIDEnv + "="
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+strconv.FormatInt(jobID, 10))
}

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

	// The pipes are ours rather than cmd.StdoutPipe()'s, so closing them is
	// this function's decision: Wait would otherwise be the only thing that
	// ever closes them, and it is exactly what is being unblocked. They are
	// created once and reused across a start retry: a start that failed never
	// touched them.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("executor: stdout pipe: %w", err)
	}
	defer func() { _ = stdout.Close() }()
	stderr, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutW.Close()
		return Result{}, fmt.Errorf("executor: stderr pipe: %w", err)
	}
	defer func() { _ = stderr.Close() }()

	var cmd *exec.Cmd
	var started time.Time
	for attempt := 0; ; attempt++ {
		cmd = exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
		cmd.Dir = spec.Dir
		cmd.Env = spec.Env
		if spec.JobID > 0 {
			cmd.Env = withJobContext(cmd.Env, spec.JobID)
		}
		// The job is the whole tree, not one pid: a cancelled run has to reach the
		// helpers its process started, and a shell that exec'd something else is
		// only reachable by group.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// An opted-in job is born inside its per-job cgroup (CLONE_INTO_CGROUP),
		// so its memory/cpu ceiling is in force from the first instruction rather
		// than applied a moment after it already started allocating.
		if spec.UseCgroup {
			applyCgroupFD(cmd.SysProcAttr, spec.CgroupFD)
		}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.Stdout, cmd.Stderr = stdoutW, stderrW

		started = time.Now()
		err = cmd.Start()
		if err == nil || !errors.Is(err, syscall.ETXTBSY) || attempt >= textBusyRetries {
			break
		}
		// Back off briefly for the sibling exec to complete, then re-arm. The
		// context still bounds the total wait.
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(textBusyBackoff(attempt)):
			continue
		}
		break
	}
	// Only the child needs the write ends; holding them here would mean no
	// reader ever sees EOF.
	_ = stdoutW.Close()
	_ = stderrW.Close()
	if err != nil {
		return Result{}, fmt.Errorf("executor: start %s: %w", spec.Argv[0], err)
	}

	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
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
	}()

	waitErr := cmd.Wait()
	drainOutput(scanned, stdout, stderr)
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

// drainOutput waits for the scanners to reach EOF, then gives up: whatever
// still holds the write end outlived the job, and the job's outcome does not
// wait on it. Closing the read ends is what stops the scanners.
func drainOutput(scanned <-chan struct{}, pipes ...*os.File) {
	timer := time.NewTimer(pipeDrainGrace)
	defer timer.Stop()

	select {
	case <-scanned:
		return
	case <-timer.C:
	}
	for _, p := range pipes {
		_ = p.Close()
	}
	<-scanned
}

// textBusyBackoff grows the pause between ETXTBSY start retries from 1ms,
// capped at 32ms: the window is a single sibling fork→exec, so a short
// exponential clears it well inside the retry budget.
func textBusyBackoff(attempt int) time.Duration {
	d := time.Millisecond << attempt
	if d > 32*time.Millisecond {
		d = 32 * time.Millisecond
	}
	return d
}

func scan(r io.Reader, level string, sink EventSink) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for scanner.Scan() {
		sink.Emit(Event{Ts: time.Now().UTC(), Level: level, Msg: scanner.Text()})
	}
	// A closed pipe is how a killed process ends, and how a drain that gave up
	// on a grandchild ends; neither is an output error.
	if err := scanner.Err(); err != nil &&
		!errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, os.ErrClosed) {
		sink.Emit(Event{
			Ts:    time.Now().UTC(),
			Level: LevelSystem,
			Msg:   fmt.Sprintf("output scan stopped: %v", err),
		})
	}
}
