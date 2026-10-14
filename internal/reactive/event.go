package reactive

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The sources this package normalizes. A source names both the shape of the
// body and the derivation id its hook token and signature key come from.
const (
	SourceAlertmanager = "alertmanager"
	SourceHealthchecks = "healthchecks"
	SourceGeneric      = "generic"
	// SourceDrift is not a webhook: it is a scheduled `tofu plan` that found
	// changes, entering the same pipeline so drift can gate an apply.
	SourceDrift = "drift"
	// SourceCheck is not a webhook: the check sweep probes targets and emits
	// one event per declared transition, entering the same pipeline a rule
	// listens to. It is deliberately absent from Sources — that list is the
	// webhook surface the ingress resolves tokens for.
	SourceCheck = "check"
	// SourceHost is not a webhook: it is a job request posted by a managed
	// host that authenticated as itself, entering the same pipeline so the
	// configured rules, guardrails, and approval gates apply to it.
	SourceHost = "host"
)

// Sources lists every source the ingress accepts, in the order tokens are
// resolved.
var Sources = []string{SourceAlertmanager, SourceHealthchecks, SourceGeneric}

// Event statuses. Every source's vocabulary collapses onto these two, because
// "the thing that was wrong is no longer wrong" is what cancels a pending
// approval, whatever the sender calls it.
const (
	StatusFiring   = "firing"
	StatusResolved = "resolved"
)

// Event is the one shape every source normalizes onto, and the value CEL
// expressions are evaluated against.
type Event struct {
	Source      string            `json:"source"`
	Kind        string            `json:"kind"`
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	Severity    string            `json:"severity"`
	Host        string            `json:"host"`
	Instance    string            `json:"instance"`
	Fingerprint string            `json:"fingerprint"`
	Summary     string            `json:"summary"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	ReceivedAt  time.Time         `json:"received_at"`
	DedupKey    string            `json:"dedup_key"`
	// CheckObservations is a check transition's hysteresis ledger (JSON) that
	// record() persists onto store.Event.CheckObservations. It is excluded from
	// the normalized body (json:"-") — the ledger is projected from the event
	// row, never from normalized_json, so there is one copy, not two.
	CheckObservations string `json:"-"`
}

// Resolved reports whether the event says the condition has cleared.
func (e Event) Resolved() bool { return e.Status == StatusResolved }

// Activation is the value a CEL expression sees as `event`. Labels and
// annotations stay maps so an expression can ask for a key the normalizer
// knows nothing about.
func (e Event) Activation() map[string]any {
	return map[string]any{
		"source":      e.Source,
		"kind":        e.Kind,
		"name":        e.Name,
		"status":      e.Status,
		"severity":    e.Severity,
		"host":        e.Host,
		"instance":    e.Instance,
		"fingerprint": e.Fingerprint,
		"summary":     e.Summary,
		"labels":      stringMap(e.Labels),
		"annotations": stringMap(e.Annotations),
	}
}

// stringMap returns a non-nil copy, so `event.labels["absent"]` is a missing
// key rather than an error about a null map.
func stringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// JSON renders the event as the normalized_json column holds it.
func (e Event) JSON() string {
	blob, err := json.Marshal(e)
	if err != nil {
		// Every field is a string, a map of strings, or a time: unreachable.
		return fmt.Sprintf(`{"source":%q,"error":"normalize: %v"}`, e.Source, err)
	}
	return string(blob)
}

// fillDedupKey derives the key that identifies "the same condition" across
// repeats and across the resolve that clears it. Status is deliberately not
// part of it: a resolved alert must find the approval its firing raised.
func (e *Event) fillDedupKey() {
	if e.DedupKey != "" {
		return
	}
	if e.Fingerprint != "" {
		e.DedupKey = e.Source + ":fp:" + e.Fingerprint
		return
	}
	parts := []string{e.Source, e.Name}
	if scope := e.scopeValue(); scope != "" {
		parts = append(parts, scope)
	}
	e.DedupKey = strings.Join(parts, ":")
}

// scopeValue is the host a host-scoped silence matches on: the explicit host,
// or the instance with its port trimmed off the way an exporter address
// carries one.
func (e Event) scopeValue() string {
	if e.Host != "" {
		return e.Host
	}
	host, _, found := strings.Cut(e.Instance, ":")
	if found {
		return host
	}
	return e.Instance
}
