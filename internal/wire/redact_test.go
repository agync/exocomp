package wire

import (
	"encoding/json"
	"testing"
)

// TestHasSensitivePlan pins the ONE redaction classifier every face shares:
// a *_sensitive tree with any `true` is secret, an empty or plain tree is not.
func TestHasSensitivePlan(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty", ``, false},
		{"null", `null`, false},
		{"plain value", `{"password":"hunter2"}`, false},
		{"sensitive true", `{"password":"hunter2","secret":true}`, true},
		{"nested sensitive", `{"data":{"token":{"secret":true}}}`, true},
		{"false only", `{"secret":false}`, false},
	}
	for _, tc := range cases {
		if got := HasSensitivePlan(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("HasSensitivePlan(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
