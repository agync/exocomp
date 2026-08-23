package executor

import (
	"strings"
	"sync"
	"time"
)

// noHostsBanner is the line ansible's default stdout callback prints for a play
// whose host pattern resolves to zero hosts (v2_playbook_on_no_hosts_matched).
// It is a fixed callback string, so matching it is a stable signal rather than a
// fragile heuristic over free-form output.
const noHostsBanner = "skipping: no hosts matched"

// noHostsObserver forwards every event and, on the side, tracks whether the run
// matched any host at all. A mis-pointed or unparseable inventory resolves to
// zero hosts, so ansible exits 0 with an empty PLAY RECAP: the run looks green
// but nothing ran. The observer notes ansible's no-hosts banner and counts the
// PLAY RECAP host rows; it retains no stdout beyond those two cheap signals, so
// it adds nothing to a normal run's memory (the hostKeyObserver pattern).
type noHostsObserver struct {
	inner   EventSink
	mu      sync.Mutex
	inRecap bool
	recap   bool // a PLAY RECAP header was printed
	hosts   int  // host rows counted inside the recap
	banner  bool // the no-hosts-matched banner was printed
}

// Emit observes stdout lines (recap and banner are callback stdout, LevelInfo)
// and forwards every event untouched. Gating recap tracking on LevelInfo keeps
// the inRecap state on the single ordered stdout stream, so an interleaved
// stderr line cannot corrupt it.
func (o *noHostsObserver) Emit(e Event) {
	if e.Level == LevelInfo {
		o.observe(e.Msg)
	}
	o.inner.Emit(e)
}

func (o *noHostsObserver) observe(line string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if strings.Contains(line, noHostsBanner) {
		o.banner = true
	}
	if strings.HasPrefix(line, "PLAY RECAP") {
		o.inRecap = true
		o.recap = true
		return
	}
	if !o.inRecap {
		return
	}
	if _, fields, ok := strings.Cut(line, ":"); ok {
		if _, ok := recapCounts(fields); ok {
			o.hosts++
		}
	}
}

// matchedNoHosts reports whether the run settled having matched zero hosts: it
// reached host selection (a recap header or the skip banner appeared) and no
// recap host row was counted. A run that errored before ansible got to host
// selection produced neither signal and is not reported as "no hosts" — it never
// got there, which is a different, non-tolerated failure.
func (o *noHostsObserver) matchedNoHosts() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.hosts > 0 {
		return false
	}
	return o.recap || o.banner
}

// noHostsMsg is the warning line a zero-host run surfaces. It names the likely
// cause so an operator scanning events sees why a green run changed nothing.
const noHostsMsg = "ansible: no hosts matched — the run changed nothing; " +
	"check the --inventory and host pattern"

// emitNoHostsOutcome emits the zero-host warning event. The run exited 0 — a
// mis-pointed inventory or a typo'd pattern parses to no hosts and ansible
// reports success — so the warning is what stops a green run that did nothing
// from passing unnoticed. The level is a warning (LevelSystem), never an error:
// a play legitimately targeting an empty group is not a failure, and rewriting
// the exit code would break AGENTS.md invariant 3. It only makes the state
// visible.
func emitNoHostsOutcome(sink EventSink) {
	sink.Emit(Event{
		Ts:    time.Now().UTC(),
		Level: LevelSystem,
		Msg:   noHostsMsg,
	})
}
