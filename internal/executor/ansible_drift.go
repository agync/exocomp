package executor

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ansibleCheckEventKey namespaces the check-mode outcome inside an event's
// raw_json, the way tofuEventKey namespaces the plan outcome.
const ansibleCheckEventKey = "ansible_check"

// AnsibleHostDrift is one host a check-mode run reports would change: its name
// and how many tasks ansible would apply were the run not a dry run. Only the
// count travels — a task's diff can hold a secret, so the resource-level signal
// is "how much", never "what to".
type AnsibleHostDrift struct {
	Host    string `json:"host"`
	Changed int    `json:"changed"`
}

// AnsibleCheckOutcome is the structured payload a `--check` run attaches to its
// event: the check-mode twin of TofuPlanOutcome. A host with changed>0 is drift
// (config would move it); a host with changed==0 is clean. Changes stays for
// readers that only ask "would anything move".
type AnsibleCheckOutcome struct {
	Operation  string             `json:"operation"` // always "check"
	Changes    bool               `json:"changes"`
	CleanHosts int                `json:"clean_hosts"`
	Drift      []AnsibleHostDrift `json:"drift,omitempty"`
}

// AnsibleCheckChanges reads the check-mode outcome back out of a job event's
// raw_json, the config-drift twin of TofuPlanChanges. The reactive engine's
// drift sweep uses it to turn a scheduled `--check` play that found drift into
// an event the same way SweepDrift turns a tofu plan into a TofuDrift event:
// scan the succeeded ansible jobs, read each one's events through this, and
// raise an AnsibleDrift event when Changes is true. It lives here, beside the
// producer, so the two cannot drift apart. ok is false when the event carries no
// ansible_check payload (a normal, non-check run).
func AnsibleCheckChanges(rawJSON string) (outcome AnsibleCheckOutcome, ok bool) {
	if rawJSON == "" {
		return AnsibleCheckOutcome{}, false
	}
	var envelope struct {
		Check *AnsibleCheckOutcome `json:"ansible_check"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &envelope); err != nil || envelope.Check == nil {
		return AnsibleCheckOutcome{}, false
	}
	return *envelope.Check, true
}

// isAnsibleCheck reports whether argv runs ansible in check mode, so a normal
// run is left exactly as it was and only a dry run grows the drift event.
func isAnsibleCheck(argv []string) bool {
	for _, a := range argv {
		if a == "--check" || a == "-C" {
			return true
		}
	}
	return false
}

// ParseAnsibleRecap reads the PLAY RECAP an ansible run prints and pulls out the
// per-host changed-task counts. It returns found=false when no recap was
// printed — a run that errored before the recap has no counts to report, and
// the caller must not pass that off as "every host clean". Parsing enriches the
// outcome; it never decides the run (AGENTS.md invariant 3).
func ParseAnsibleRecap(lines []string) (outcome AnsibleCheckOutcome, found bool) {
	outcome.Operation = "check"
	inRecap := false
	for _, line := range lines {
		if strings.HasPrefix(line, "PLAY RECAP") {
			inRecap = true
			found = true
			continue
		}
		if !inRecap {
			continue
		}
		host, fields, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		changed, ok := recapChanged(fields)
		if !ok {
			// A blank separator or a trailing banner line is not a host row.
			continue
		}
		if changed > 0 {
			outcome.Drift = append(outcome.Drift, AnsibleHostDrift{Host: host, Changed: changed})
			outcome.Changes = true
		} else {
			outcome.CleanHosts++
		}
	}
	return outcome, found
}

// recapChanged extracts the changed=N count from a recap host row's fields. A
// row that carries no changed= token (a stray line inside the recap block) is
// reported as not a host row.
func recapChanged(fields string) (int, bool) {
	seenChanged := false
	changed := 0
	for _, tok := range strings.Fields(fields) {
		key, val, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			continue
		}
		if key == "changed" {
			changed = n
			seenChanged = true
		}
	}
	return changed, seenChanged
}

// ansibleCheckMsg is the human line beside the structured payload:
// 'N hosts clean, host X: M tasks would change'.
func ansibleCheckMsg(o AnsibleCheckOutcome) string {
	base := fmt.Sprintf("ansible check: %d host(s) clean", o.CleanHosts)
	if len(o.Drift) == 0 {
		return base
	}
	parts := make([]string, len(o.Drift))
	for i, d := range o.Drift {
		parts[i] = fmt.Sprintf("host %s: %d task(s) would change", d.Host, d.Changed)
	}
	return base + ", " + strings.Join(parts, ", ")
}

// emitAnsibleCheckOutcome parses the collected stdout for a recap and, when one
// was printed, emits the structured drift event.
func emitAnsibleCheckOutcome(lines []string, sink EventSink) {
	outcome, found := ParseAnsibleRecap(lines)
	if !found {
		return
	}
	raw, err := json.Marshal(map[string]AnsibleCheckOutcome{ansibleCheckEventKey: outcome})
	if err != nil {
		// Unreachable: the payload is fixed fields and string slices.
		panic("executor: ansible: encode check outcome: " + err.Error())
	}
	sink.Emit(Event{
		Ts:      time.Now().UTC(),
		Level:   LevelSystem,
		Msg:     ansibleCheckMsg(outcome),
		RawJSON: string(raw),
	})
}

// recapSink forwards every event to the inner sink and, on the side, keeps the
// stdout (LevelInfo) lines so the driver can parse the PLAY RECAP after the run.
// Only a check-mode run wraps the sink, so a normal run streams exactly as
// before with nothing retained.
type recapSink struct {
	inner EventSink
	mu    sync.Mutex
	lines []string
}

func (r *recapSink) Emit(e Event) {
	if e.Level == LevelInfo {
		r.mu.Lock()
		r.lines = append(r.lines, e.Msg)
		r.mu.Unlock()
	}
	r.inner.Emit(e)
}

func (r *recapSink) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}
