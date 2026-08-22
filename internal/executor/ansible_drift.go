package executor

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ansibleCheckEventKey namespaces the check-mode outcome inside an event's
// raw_json, the way tofuEventKey namespaces the plan outcome.
const ansibleCheckEventKey = "ansible_check"

// ansiblePartialEventKey namespaces the unreachable-tolerant outcome inside an
// event's raw_json. It rides beside ansible_check: the check twin records "what
// would change", this one records "who was offline but tolerated".
const ansiblePartialEventKey = "ansible_partial"

// AnsiblePartialOutcome is the structured payload a `--tolerate-unreachable` run
// attaches to a job event when its only non-success is unreachable hosts: the
// online hosts converged, and the named hosts were offline and recorded rather
// than paged. Only host names travel — a degradation is public run state, not a
// secret.
type AnsiblePartialOutcome struct {
	Operation   string   `json:"operation"` // always "run"
	Unreachable []string `json:"unreachable"`
}

// AnsiblePartialHosts reads the partial outcome back out of a job event's
// raw_json, the twin of AnsibleCheckChanges: a reader (the CLI, a future notify
// sweep) turns a degraded run's event into the list of offline hosts. ok is
// false when the event carries no ansible_partial payload.
func AnsiblePartialHosts(rawJSON string) (outcome AnsiblePartialOutcome, ok bool) {
	if rawJSON == "" {
		return AnsiblePartialOutcome{}, false
	}
	var envelope struct {
		Partial *AnsiblePartialOutcome `json:"ansible_partial"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &envelope); err != nil || envelope.Partial == nil {
		return AnsiblePartialOutcome{}, false
	}
	return *envelope.Partial, true
}

// partialOutcomeMsg is the warning line beside the structured payload. It names
// the offline hosts so an operator scanning events sees exactly what degraded.
func partialOutcomeMsg(hosts []string) string {
	return fmt.Sprintf("ansible: degraded — %d host(s) unreachable, tolerated: %s "+
		"(reachable hosts converged)", len(hosts), strings.Join(hosts, ", "))
}

// emitPartialOutcome emits the degraded warning event and its structured
// payload. The level is a warning, never an error: the run did not fail, it
// converged the hosts it could reach and recorded the ones it could not.
func emitPartialOutcome(hosts []string, sink EventSink) {
	outcome := AnsiblePartialOutcome{Operation: "run", Unreachable: hosts}
	raw, err := json.Marshal(map[string]AnsiblePartialOutcome{ansiblePartialEventKey: outcome})
	if err != nil {
		// Unreachable: the payload is a fixed field and a string slice.
		panic("executor: ansible: encode partial outcome: " + err.Error())
	}
	sink.Emit(Event{
		Ts:      time.Now().UTC(),
		Level:   LevelSystem,
		Msg:     partialOutcomeMsg(hosts),
		RawJSON: string(raw),
	})
}

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

// RecapHost is one PLAY RECAP row's per-host counts: the fields a run's
// classification reads. changed drives the check-mode drift outcome; unreachable
// and failed separate a tolerable degradation (a host the run never reached)
// from a real task failure (a host it reached and could not converge).
type RecapHost struct {
	Host        string
	Changed     int
	Unreachable int
	Failed      int
}

// ParseRecapHosts reads the PLAY RECAP an ansible run prints and returns one
// RecapHost per host row. found is false when no recap was printed — a run that
// errored before the recap has no counts to report, and the caller must not pass
// that off as "every host clean". Parsing enriches the outcome; it never decides
// the run (AGENTS.md invariant 3).
func ParseRecapHosts(lines []string) (hosts []RecapHost, found bool) {
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
		counts, ok := recapCounts(fields)
		if !ok {
			// A blank separator or a trailing banner line is not a host row.
			continue
		}
		counts.Host = host
		hosts = append(hosts, counts)
	}
	return hosts, found
}

// ParseAnsibleRecap reads the PLAY RECAP and pulls out the per-host changed-task
// counts for the check-mode drift outcome. It is the config-drift twin's parser;
// it reuses ParseRecapHosts so one tokenizer reads every recap.
func ParseAnsibleRecap(lines []string) (outcome AnsibleCheckOutcome, found bool) {
	outcome.Operation = "check"
	hosts, found := ParseRecapHosts(lines)
	for _, h := range hosts {
		if h.Changed > 0 {
			outcome.Drift = append(outcome.Drift, AnsibleHostDrift{Host: h.Host, Changed: h.Changed})
			outcome.Changes = true
		} else {
			outcome.CleanHosts++
		}
	}
	return outcome, found
}

// recapCounts extracts the changed=/unreachable=/failed= counts from a recap
// host row's fields. A row that carries none of ansible's recap tokens (a stray
// line inside the recap block) is reported as not a host row. A genuine recap
// row always carries at least ok=, so seeing any recap token marks it a host row.
func recapCounts(fields string) (RecapHost, bool) {
	var counts RecapHost
	seen := false
	for _, tok := range strings.Fields(fields) {
		key, val, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			continue
		}
		switch key {
		case "ok", "changed", "unreachable", "failed", "skipped", "rescued", "ignored":
			seen = true
		}
		switch key {
		case "changed":
			counts.Changed = n
		case "unreachable":
			counts.Unreachable = n
		case "failed":
			counts.Failed = n
		}
	}
	return counts, seen
}

// RunClassification separates a run's non-success hosts by cause: a host the run
// could not reach (unreachable) versus a host it reached and a task failed on
// (failed). It is the input to the unreachable-tolerant decision — a run whose
// only non-success is unreachable hosts is a tolerable degradation, while any
// failed host is a hard failure regardless of the flag.
type RunClassification struct {
	Unreachable []string
	Failed      []string
}

// ClassifyRun reads the PLAY RECAP and names the unreachable and the task-failed
// hosts. found is false when no recap was printed, in which case the caller must
// fail closed rather than treat an early error as a tolerable degradation.
func ClassifyRun(lines []string) (RunClassification, bool) {
	hosts, found := ParseRecapHosts(lines)
	if !found {
		return RunClassification{}, false
	}
	var c RunClassification
	for _, h := range hosts {
		if h.Failed > 0 {
			c.Failed = append(c.Failed, h.Host)
		}
		if h.Unreachable > 0 {
			c.Unreachable = append(c.Unreachable, h.Host)
		}
	}
	return c, true
}

// ClassifyOutcome names a run's unreachable and failed hosts from whatever
// per-host record it printed: a playbook run's PLAY RECAP, or — when no recap was
// printed — an ad-hoc `ansible` run's `host | STATUS` result lines, which carry
// no recap of their own. found is false when neither form was seen, so a run
// that errored before any host result fails closed rather than reading as clean.
func ClassifyOutcome(lines []string) (RunClassification, bool) {
	if c, ok := ClassifyRun(lines); ok {
		return c, true
	}
	return classifyAdhoc(lines)
}

// classifyAdhoc reads the per-host result lines the ad-hoc `ansible` binary
// prints — `<host> | UNREACHABLE! => …`, `<host> | FAILED | rc=2 >>`, `<host> |
// SUCCESS => …` — since ad-hoc mode prints no PLAY RECAP. found is true once any
// host result line was seen, the ad-hoc equivalent of a recap having printed.
func classifyAdhoc(lines []string) (RunClassification, bool) {
	var c RunClassification
	seen := false
	for _, line := range lines {
		host, status, ok := strings.Cut(line, " | ")
		if !ok {
			continue
		}
		// A host result header is `<host> | STATUS`; a JSON body line that
		// happens to contain " | " has leading whitespace or spaces in its left
		// half, so a host token with any whitespace is not one.
		if host == "" || strings.IndexFunc(host, unicode.IsSpace) >= 0 {
			continue
		}
		switch {
		case strings.HasPrefix(status, "UNREACHABLE"):
			c.Unreachable = append(c.Unreachable, host)
			seen = true
		case strings.HasPrefix(status, "FAILED"):
			c.Failed = append(c.Failed, host)
			seen = true
		case strings.HasPrefix(status, "SUCCESS"), strings.HasPrefix(status, "CHANGED"):
			seen = true
		}
	}
	return c, seen
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
