package executor

import (
	"context"
	"time"
)

// Event levels. They match the levels the store writes to job_event.
const (
	LevelInfo   = "info"
	LevelError  = "error"
	LevelSystem = "system"
)

// Spec is one unit of work handed to a driver.
type Spec struct {
	JobID int64
	Kind  string   // shell|ansible|tofu
	Argv  []string // Argv[0] is the executable
	Dir   string   // working directory; empty means the caller's
	// DirReadOnly grants Dir read access only under the sandbox, instead of the
	// read+write a working directory otherwise gets. It is how a read-only path
	// source (store.Source.Writable false) denies a run writes to its tree. Off
	// by default: the zero-value Spec write-grants its own directory as before.
	DirReadOnly bool
	Env         []string // nil inherits the caller's environment
	Timeout     time.Duration
	// UseCgroup places the process into the cgroup named by CgroupFD at fork,
	// so a capped job starts already inside its resource ceiling. Off by
	// default: the zero-value Spec runs uncapped exactly as before. CgroupFD is
	// consulted only when UseCgroup is true, which is why fd 0 is never mistaken
	// for a cgroup.
	UseCgroup bool
	CgroupFD  int
	// Privileged runs this one job UNSANDBOXED — no Landlock/nono policy, full
	// host access — even when the daemon's effective tier is enforcing. It is a
	// per-spec, operator-gated opt-out layered on top of the process-global tier
	// resolver, never a change to it: a sandboxed driver honours it by running
	// its plain inner driver instead of wrapping it. Off by default, so the
	// zero-value Spec keeps the daemon's effective tier exactly as before.
	Privileged bool
	// EgressPorts are extra outbound TCP ports this one job may open on top of
	// the ports its kind grants. The ansible sandboxed driver folds them into
	// the profile's ConnectPorts, so a job can reach an inventory host on a
	// nonstandard SSH port. Empty is the default posture: no declaration, no
	// widening — the profile stays the kind's own.
	EgressPorts []int
	// SSHIdentityFiles are single ssh identity key files this one job may read
	// on top of the files its kind grants. The ansible sandboxed driver folds
	// them into the profile's ReadFiles, so a play can load a key stored
	// outside the source checkout. Empty is the default posture: no
	// declaration, no widening.
	SSHIdentityFiles []string
}

// Result is what a driver reports about a process that ran to completion.
// ExitCode alone decides success (AGENTS.md invariant 3); the other fields
// only explain how the process ended.
type Result struct {
	ExitCode int  // -1 when a signal ended the process
	Signaled bool // true when a signal, not exit(), ended it
	TimedOut bool // true when the deadline killed it
	Duration time.Duration
	// Degraded is set by a driver that ran under an opt-in tolerance (the
	// ansible unreachable-tolerant run): the process exited non-zero, but the
	// only non-success was a tolerated condition, so the queue settles the job
	// as a non-failing degraded outcome rather than a failure. ExitCode still
	// carries the process's true code (invariant 3: the number is not rewritten);
	// Degraded only tells the queue how to classify it. Off by default.
	Degraded bool
	// UnreachableHosts names the hosts a Degraded ansible run could not reach.
	// It is telemetry for the job event and the operator, never the arbiter.
	UnreachableHosts []string
}

// Event is one line of a job's output or one lifecycle note.
type Event struct {
	Ts      time.Time
	Level   string
	Msg     string
	RawJSON string // structured payload for drivers that emit one
}

// EventSink receives events as a job runs. Implementations must be safe for
// concurrent use: a driver scans stdout and stderr on separate goroutines.
// Emit returns nothing because event delivery is telemetry, never the
// arbiter of whether a job succeeded.
type EventSink interface {
	Emit(Event)
}

// SinkFunc adapts a function to EventSink.
type SinkFunc func(Event)

// Emit implements EventSink.
func (f SinkFunc) Emit(e Event) { f(e) }

// Driver runs a Spec somewhere: a local subprocess today, a sandboxed one or
// a sibling container later. Run returns an error only when the process could
// not be run to completion; a process that ran and failed returns a Result
// with a non-zero ExitCode and a nil error.
type Driver interface {
	Run(ctx context.Context, spec Spec, sink EventSink) (Result, error)
}
