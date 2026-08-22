package reactive

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// maxLabels caps how many keys a normalized event carries out of a body. A
// sender with a runaway label set must not turn one webhook into an unbounded
// row.
const maxLabels = 128

// Normalize turns one posted body into the events it describes. One
// Alertmanager POST carries a group, so the result is a slice: each alert in
// the group is its own event with its own dedup key and outcome.
//
// A body that does not parse is an error, not an empty result: the sender gets
// a 400 and the operator gets nothing pretending to be an event.
func Normalize(source string, body []byte, now time.Time) ([]Event, error) {
	switch source {
	case SourceAlertmanager:
		return normalizeAlertmanager(body, now)
	case SourceHealthchecks:
		return normalizeHealthchecks(body, now)
	case SourceGeneric:
		return normalizeGeneric(body, now)
	default:
		return nil, fmt.Errorf("reactive: unknown source %q", source)
	}
}

// alertmanagerPayload is the fields of Alertmanager's v4 webhook this reads.
// Everything else stays in raw_json.
type alertmanagerPayload struct {
	Status string `json:"status"`
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Fingerprint string            `json:"fingerprint"`
	} `json:"alerts"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
}

func normalizeAlertmanager(body []byte, now time.Time) ([]Event, error) {
	var payload alertmanagerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("reactive: alertmanager body: %w", err)
	}
	if len(payload.Alerts) == 0 {
		return nil, fmt.Errorf("reactive: alertmanager body carries no alerts")
	}

	out := make([]Event, 0, len(payload.Alerts))
	for _, a := range payload.Alerts {
		labels := mergeStrings(payload.CommonLabels, a.Labels)
		annotations := mergeStrings(payload.CommonAnnotations, a.Annotations)
		status := a.Status
		if status == "" {
			status = payload.Status
		}

		e := Event{
			Source:      SourceAlertmanager,
			Kind:        "alert",
			Name:        labels["alertname"],
			Status:      normalizeStatus(status),
			Severity:    labels["severity"],
			Host:        firstOf(labels, "host", "hostname", "node"),
			Instance:    labels["instance"],
			Fingerprint: a.Fingerprint,
			Summary:     firstOf(annotations, "summary", "description", "title"),
			Labels:      labels,
			Annotations: annotations,
			ReceivedAt:  now,
		}
		e.fillDedupKey()
		out = append(out, e)
	}
	return out, nil
}

// healthchecksPayload covers both shapes a healthchecks instance posts: the
// check object nested under "check", and the same fields flattened.
type healthchecksPayload struct {
	Check *healthchecksCheck `json:"check"`
	healthchecksCheck
}

type healthchecksCheck struct {
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Status    string `json:"status"`
	Tags      string `json:"tags"`
	UniqueKey string `json:"unique_key"`
}

func normalizeHealthchecks(body []byte, now time.Time) ([]Event, error) {
	var payload healthchecksPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("reactive: healthchecks body: %w", err)
	}
	check := payload.healthchecksCheck
	if payload.Check != nil {
		check = *payload.Check
	}
	if check.Name == "" && check.Slug == "" {
		return nil, fmt.Errorf("reactive: healthchecks body names no check")
	}

	name := check.Name
	if name == "" {
		name = check.Slug
	}
	labels := map[string]string{"check": name, "status": check.Status}
	if check.Slug != "" {
		labels["slug"] = check.Slug
	}
	for _, tag := range strings.Fields(check.Tags) {
		key, value, found := strings.Cut(tag, "=")
		if !found {
			labels[key] = "true"
			continue
		}
		labels[key] = value
	}

	e := Event{
		Source:      SourceHealthchecks,
		Kind:        "check",
		Name:        name,
		Status:      normalizeStatus(check.Status),
		Severity:    "critical",
		Host:        labels["host"],
		Fingerprint: check.UniqueKey,
		Summary:     "check " + name + " is " + check.Status,
		Labels:      capLabels(labels),
		Annotations: map[string]string{},
		ReceivedAt:  now,
	}
	e.fillDedupKey()
	return []Event{e}, nil
}

// normalizeGeneric accepts any JSON object. Top-level scalars become labels,
// so a sender nobody wrote an adapter for is still addressable from CEL.
func normalizeGeneric(body []byte, now time.Time) ([]Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("reactive: generic body: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("reactive: generic body is an empty object")
	}

	labels := map[string]string{}
	for k, v := range raw {
		switch value := v.(type) {
		case string:
			labels[k] = value
		case bool:
			labels[k] = fmt.Sprint(value)
		case float64:
			labels[k] = fmt.Sprintf("%g", value)
		}
	}

	e := Event{
		Source:      SourceGeneric,
		Kind:        "generic",
		Name:        firstOf(labels, "name", "alertname", "title", "event"),
		Status:      normalizeStatus(labels["status"]),
		Severity:    firstOf(labels, "severity", "priority", "level"),
		Host:        firstOf(labels, "host", "hostname", "node"),
		Instance:    labels["instance"],
		Fingerprint: labels["fingerprint"],
		Summary:     firstOf(labels, "summary", "message", "description"),
		Labels:      capLabels(labels),
		Annotations: map[string]string{},
		ReceivedAt:  now,
	}
	if e.Name == "" {
		return nil, fmt.Errorf("reactive: generic body has no name, alertname, title, or event")
	}
	e.fillDedupKey()
	return []Event{e}, nil
}

// normalizeStatus collapses each source's vocabulary onto firing/resolved.
// Anything unrecognized is firing: a monitor saying something this code has
// not seen before is not evidence that the problem went away.
func normalizeStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "resolved", "ok", "up", "success", "healthy", "cleared":
		return StatusResolved
	default:
		return StatusFiring
	}
}

func mergeStrings(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return capLabels(out)
}

// capLabels bounds a label set, keeping the keys a rule is most likely to
// name. Dropping is silent in the normalized shape and visible in raw_json,
// which keeps the whole body available for forensics.
func capLabels(m map[string]string) map[string]string {
	if len(m) <= maxLabels {
		return m
	}
	keys := slices.Sorted(maps.Keys(m))
	out := make(map[string]string, maxLabels)
	for _, k := range keys[:maxLabels] {
		out[k] = m[k]
	}
	return out
}

func firstOf(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}
