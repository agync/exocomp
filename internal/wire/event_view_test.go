package wire

import (
	"encoding/json"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// TestEventViewCheckFields: the additive name/status/netgroup fields project a
// check transition's normalized body — the check name, the direction the sweep
// observed, and the location verdict from labels.netgroup — and stay empty for
// alert events, whose bodies carry the same keys but different semantics.
func TestEventViewCheckFields(t *testing.T) {
	cases := []struct {
		name     string
		ev       store.Event
		wantName string
		wantSt   string
		wantNg   string
	}{
		{"check firing in home netgroup", store.Event{
			Source: reactive.SourceCheck,
			Normalized: `{"source":"check","kind":"check","name":"at-home","status":"firing",` +
				`"severity":"warning","host":"laptop","labels":{"target":"laptop","netgroup":"home"}}`,
		}, "at-home", "firing", "home"},
		{"check resolved away", store.Event{
			Source: reactive.SourceCheck,
			Normalized: `{"source":"check","kind":"check","name":"at-home","status":"resolved",` +
				`"severity":"warning","host":"laptop","labels":{"target":"laptop","netgroup":"away"}}`,
		}, "at-home", "resolved", "away"},
		{"check http url target carries no netgroup verdict", store.Event{
			Source: reactive.SourceCheck,
			Normalized: `{"source":"check","kind":"check","name":"portal","status":"firing",` +
				`"severity":"warning","host":"https://portal.lan","labels":{"target":"https://portal.lan"}}`,
		}, "portal", "firing", ""},
		{"alert event leaves check fields empty", store.Event{
			Source: reactive.SourceAlertmanager,
			Normalized: `{"source":"alertmanager","kind":"alert","name":"DiskFull","status":"firing",` +
				`"severity":"critical","host":"nas","labels":{"netgroup":"home"}}`,
		}, "DiskFull", "", ""},
	}
	for _, tc := range cases {
		v := NewEventView(tc.ev)
		if v.Name != tc.wantName || v.Status != tc.wantSt || v.Netgroup != tc.wantNg {
			t.Errorf("%s: view = {name:%q status:%q netgroup:%q}, want {name:%q status:%q netgroup:%q}",
				tc.name, v.Name, v.Status, v.Netgroup, tc.wantName, tc.wantSt, tc.wantNg)
		}
	}
}

// TestEventViewOmitEmptyJSON pins the wire shape the mobile companion consumes:
// a check row carries name/status/netgroup keys, an alert row omits status and
// netgroup entirely (additive omitempty, no breaking change for existing rows).
func TestEventViewOmitEmptyJSON(t *testing.T) {
	check, err := json.Marshal(NewEventView(store.Event{
		Source: reactive.SourceCheck,
		Normalized: `{"source":"check","kind":"check","name":"at-home","status":"firing",` +
			`"severity":"warning","host":"laptop","labels":{"target":"laptop","netgroup":"home"}}`,
		CheckObservations: `[{"observed_at":"2026-09-01T13:41:15Z","status":"up","consecutive":1},` +
			`{"observed_at":"2026-09-01T13:41:30Z","status":"up","consecutive":2},` +
			`{"observed_at":"2026-09-01T13:41:45Z","status":"up","consecutive":3}]`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name":"at-home"`, `"status":"firing"`, `"netgroup":"home"`,
		`"check_observations"`, `"observed_at":"2026-09-01T13:41:45Z"`, `"consecutive":3`} {
		if !strings.Contains(string(check), want) {
			t.Fatalf("check view %s missing %s", check, want)
		}
	}

	alert, err := json.Marshal(NewEventView(store.Event{
		Source: reactive.SourceAlertmanager,
		Normalized: `{"source":"alertmanager","kind":"alert","name":"DiskFull",` +
			`"status":"firing","severity":"critical","host":"nas"}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(alert); strings.Contains(s, "status") || strings.Contains(s, "netgroup") {
		t.Fatalf("alert view %s must omit status and netgroup", s)
	}
	if strings.Contains(string(alert), "check_observations") {
		t.Fatalf("alert view %s must omit check_observations", alert)
	}
	if !strings.Contains(string(alert), `"name":"DiskFull"`) {
		t.Fatalf("alert view %s must carry its name", alert)
	}
}

// TestEventViewUnparseableBody: a body that does not parse projects the source
// as the name and empty check fields, mirroring the web's error-tolerant read.
func TestEventViewUnparseableBody(t *testing.T) {
	v := NewEventView(store.Event{Source: reactive.SourceCheck, Normalized: "not-json"})
	if v.Name != reactive.SourceCheck || v.Status != "" || v.Netgroup != "" {
		t.Fatalf("unparseable body view = %+v, want source-only", v)
	}
}
