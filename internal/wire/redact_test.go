package wire

import (
	"encoding/json"
	"testing"
)

// TestHasSensitiveValue pins the ONE redaction classifier every face shares. It
// is value-aware: a `true` leaf in the *_sensitive tree counts only when the
// value it mirrors is set. A provider that marks a schema attribute sensitive
// whether or not it is used (hashicorp/local's `sensitive_content`) must not
// redact a resource whose value is unset.
func TestHasSensitiveValue(t *testing.T) {
	obj := func(s string) map[string]any {
		var v map[string]any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatalf("bad fixture %q: %v", s, err)
		}
		return v
	}
	cases := []struct {
		name      string
		sensitive string
		value     any
		want      bool
	}{
		{"empty tree", ``, obj(`{"a":1}`), false},
		{"null tree", `null`, obj(`{"a":1}`), false},
		{"plain value", `{"password":"hunter2"}`, obj(`{"password":"hunter2"}`), false},
		{"true with a set value", `{"password":true}`, obj(`{"password":"hunter2"}`), true},
		{"true with a null value", `{"sensitive_content":true}`,
			obj(`{"content":"plain","sensitive_content":null}`), false},
		{"true with an absent value", `{"sensitive_content":true}`, obj(`{"content":"plain"}`), false},
		{"false only", `{"password":false}`, obj(`{"password":"x"}`), false},
		{"nested and set", `{"data":{"token":true}}`, obj(`{"data":{"token":"abc"}}`), true},
		{"nested and unset", `{"data":{"token":true}}`, obj(`{"data":{}}`), false},
		{"whole object true", `true`, obj(`{"a":1}`), true},
		{"whole object true, no value", `true`, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasSensitiveValue(json.RawMessage(tc.sensitive), tc.value); got != tc.want {
				t.Errorf("HasSensitiveValue(%q, %v) = %v, want %v", tc.sensitive, tc.value, got, tc.want)
			}
		})
	}
}
