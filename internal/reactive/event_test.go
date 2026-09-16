package reactive

import (
	"testing"
	"time"
)

// TestWebhookEventCarriesNoHosts pins the trusted-source line at the
// normalizer: no webhook source ever produces a host set, even a generic body
// whose top-level keys would otherwise become labels. Only the sweep sets
// Hosts, so a rule can never be made to run over attacker-supplied hosts.
func TestWebhookEventCarriesNoHosts(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		source string
		body   string
	}{
		{SourceAlertmanager, `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"DiskFull"}}]}`},
		{SourceHealthchecks, `{"name":"db","status":"down"}`},
		{SourceGeneric, `{"name":"x","status":"up","host":"nas","hosts":"evil,host"}`},
	}
	for _, tc := range cases {
		events, err := Normalize(tc.source, []byte(tc.body), now)
		if err != nil {
			t.Fatalf("%s normalize: %v", tc.source, err)
		}
		if len(events) == 0 {
			t.Fatalf("%s produced no events", tc.source)
		}
		for _, ev := range events {
			if len(ev.Hosts) != 0 {
				t.Fatalf("%s event carries hosts %v; a webhook normalizer must leave it empty", tc.source, ev.Hosts)
			}
			if hosts, ok := ev.Activation()["hosts"].([]string); !ok || len(hosts) != 0 {
				t.Fatalf("%s activation hosts = %v, want an empty list", tc.source, ev.Activation()["hosts"])
			}
		}
	}
}

// TestEventActivationCarriesHosts pins the CEL surface: a sweep event's host
// set is readable as event.hosts, and an event with none reads as an empty list
// rather than null.
func TestEventActivationCarriesHosts(t *testing.T) {
	hosts, ok := Event{Hosts: []string{"laptop", "nas"}}.Activation()["hosts"].([]string)
	if !ok || len(hosts) != 2 || hosts[0] != "laptop" || hosts[1] != "nas" {
		t.Fatalf("activation hosts = %v, want [laptop nas]", hosts)
	}
	empty, ok := Event{}.Activation()["hosts"].([]string)
	if !ok || len(empty) != 0 {
		t.Fatalf("absent hosts activation = %v, want an empty list", empty)
	}
}
