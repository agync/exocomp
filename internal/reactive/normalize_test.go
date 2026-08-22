package reactive_test

import (
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/reactive"
)

func TestNormalizeAlertmanagerGroup(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	body := []byte(`{
	  "version": "4", "status": "firing", "receiver": "exocomp",
	  "commonLabels": {"severity": "critical", "cluster": "home"},
	  "commonAnnotations": {"runbook": "docs/disk.md"},
	  "alerts": [
	    {"status": "firing",
	     "labels": {"alertname": "DiskFull", "instance": "web-01:9100"},
	     "annotations": {"summary": "the disk is full"},
	     "fingerprint": "abc"},
	    {"status": "resolved",
	     "labels": {"alertname": "CertExpiring", "host": "gw", "severity": "warning"},
	     "annotations": {},
	     "fingerprint": "def"}
	  ]
	}`)

	events, err := reactive.Normalize(reactive.SourceAlertmanager, body, now)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("normalized %d events, want one per alert in the group", len(events))
	}

	first := events[0]
	if first.Name != "DiskFull" || first.Status != reactive.StatusFiring {
		t.Fatalf("first event = %+v", first)
	}
	if first.Severity != "critical" {
		t.Fatalf("severity = %q, want the common label to apply", first.Severity)
	}
	if first.Instance != "web-01:9100" {
		t.Fatalf("instance = %q", first.Instance)
	}
	if first.Summary != "the disk is full" {
		t.Fatalf("summary = %q", first.Summary)
	}
	if first.Labels["cluster"] != "home" || first.Annotations["runbook"] != "docs/disk.md" {
		t.Fatalf("common labels and annotations did not merge: %+v", first)
	}
	if first.DedupKey != "alertmanager:fp:abc" {
		t.Fatalf("dedup key = %q, want the fingerprint", first.DedupKey)
	}

	second := events[1]
	if second.Status != reactive.StatusResolved || !second.Resolved() {
		t.Fatalf("second event status = %q, want resolved", second.Status)
	}
	// Per-alert labels win over the common ones.
	if second.Severity != "warning" {
		t.Fatalf("second severity = %q, want the per-alert label", second.Severity)
	}
}

func TestNormalizeHealthchecks(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	nested := []byte(`{"check": {"name": "nightly-backup", "slug": "nightly-backup",
	                            "status": "down", "tags": "prod host=nas",
	                            "unique_key": "u-1"}}`)
	events, err := reactive.Normalize(reactive.SourceHealthchecks, nested, now)
	if err != nil {
		t.Fatalf("normalize nested: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("normalized %d events, want 1", len(events))
	}
	got := events[0]
	if got.Name != "nightly-backup" || got.Status != reactive.StatusFiring {
		t.Fatalf("event = %+v, want a firing check", got)
	}
	if got.Host != "nas" || got.Labels["prod"] != "true" {
		t.Fatalf("tags did not become labels: %+v", got.Labels)
	}
	if got.DedupKey != "healthchecks:fp:u-1" {
		t.Fatalf("dedup key = %q", got.DedupKey)
	}

	flat := []byte(`{"name": "nightly-backup", "status": "up"}`)
	events, err = reactive.Normalize(reactive.SourceHealthchecks, flat, now)
	if err != nil {
		t.Fatalf("normalize flat: %v", err)
	}
	if events[0].Status != reactive.StatusResolved {
		t.Fatalf("status = %q, want up to be resolved", events[0].Status)
	}
	// Firing and resolved for one check share a key, which is what lets the
	// resolve cancel what the failure raised.
	if events[0].DedupKey != "healthchecks:nightly-backup" {
		t.Fatalf("dedup key = %q", events[0].DedupKey)
	}
}

func TestNormalizeGeneric(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	body := []byte(`{"title": "TankDegraded", "severity": "critical",
	                 "host": "nas", "pool": "tank", "disks": 4, "scrubbing": false}`)
	events, err := reactive.Normalize(reactive.SourceGeneric, body, now)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	got := events[0]
	if got.Name != "TankDegraded" || got.Host != "nas" || got.Severity != "critical" {
		t.Fatalf("event = %+v", got)
	}
	// Scalars a normalizer knows nothing about are still addressable from CEL.
	if got.Labels["pool"] != "tank" || got.Labels["disks"] != "4" || got.Labels["scrubbing"] != "false" {
		t.Fatalf("labels = %+v", got.Labels)
	}
	if got.Status != reactive.StatusFiring {
		t.Fatalf("status = %q, want an unknown status to be firing", got.Status)
	}
}

// TestNormalizeRefusesWhatItCannotRead: a body that does not parse is an
// error, not an empty result pretending nothing arrived.
func TestNormalizeRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	for _, tc := range []struct {
		name   string
		source string
		body   string
	}{
		{"alertmanager with no alerts", reactive.SourceAlertmanager, `{"status":"firing","alerts":[]}`},
		{"alertmanager garbage", reactive.SourceAlertmanager, `not json`},
		{"healthchecks with no check", reactive.SourceHealthchecks, `{"status":"down"}`},
		{"generic with no name", reactive.SourceGeneric, `{"severity":"critical"}`},
		{"generic empty object", reactive.SourceGeneric, `{}`},
		{"unknown source", "nagios", `{"name":"x"}`},
	} {
		if _, err := reactive.Normalize(tc.source, []byte(tc.body), now); err == nil {
			t.Fatalf("%s: normalized, want a refusal", tc.name)
		}
	}
}

// TestUnknownStatusIsFiring: a monitor saying something this code has not seen
// before is not evidence that the problem went away.
func TestUnknownStatusIsFiring(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	events, err := reactive.Normalize(reactive.SourceGeneric,
		[]byte(`{"name":"Weird","status":"flapping"}`), now)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if events[0].Status != reactive.StatusFiring {
		t.Fatalf("status = %q, want firing", events[0].Status)
	}
}

func TestHookTokensAreDistinctPerSource(t *testing.T) {
	t.Parallel()
	d := testDeriver{seed: "hook-test"}

	seen := map[string]string{}
	for _, source := range reactive.Sources {
		token := reactive.HookToken(d, source)
		if len(token) != 20 {
			t.Fatalf("token for %s is %d characters", source, len(token))
		}
		if other, dup := seen[token]; dup {
			t.Fatalf("sources %s and %s share a hook token", other, source)
		}
		seen[token] = source
	}

	// A different envelope means different tokens: they are derived, not
	// stored, so a restore rebuilds them and a rotation invalidates them.
	other := testDeriver{seed: "another-envelope"}
	if reactive.HookToken(d, reactive.SourceAlertmanager) ==
		reactive.HookToken(other, reactive.SourceAlertmanager) {
		t.Fatal("two envelopes derived the same hook token")
	}
}

func TestSignBodyRoundTrips(t *testing.T) {
	t.Parallel()
	d := testDeriver{seed: "sig-test"}
	body := []byte(`{"alerts":[]}`)

	header := reactive.SignBody(d, reactive.SourceAlertmanager, body)
	if !strings.HasPrefix(header, "sha256=") {
		t.Fatalf("signature header = %q", header)
	}
	if err := reactive.VerifyBodySignature(d, reactive.SourceAlertmanager, header, body); err != nil {
		t.Fatalf("verify own signature: %v", err)
	}
	if err := reactive.VerifyBodySignature(d, reactive.SourceGeneric, header, body); err == nil {
		t.Fatal("a signature verified under another source's key")
	}
	if err := reactive.VerifyBodySignature(d, reactive.SourceAlertmanager, header, []byte(`{}`)); err == nil {
		t.Fatal("a signature verified over a different body")
	}
	for _, bad := range []string{"", "deadbeef", "sha256=zz", "md5=abc"} {
		if err := reactive.VerifyBodySignature(d, reactive.SourceAlertmanager, bad, body); err == nil {
			t.Fatalf("malformed signature %q verified", bad)
		}
	}
}
