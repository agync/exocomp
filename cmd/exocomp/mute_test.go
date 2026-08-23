package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestMuteListTextUnchanged pins the empty-state text, bare vs --output text.
func TestMuteListTextUnchanged(t *testing.T) {
	seedStore(t)

	bare, err := run(t, "mute", "list")
	if err != nil {
		t.Fatal(err)
	}
	if bare != "no active mutes\n" {
		t.Fatalf("bare = %q", bare)
	}
	explicit, err := run(t, "mute", "list", "--output", "text")
	if err != nil {
		t.Fatal(err)
	}
	if explicit != bare {
		t.Fatalf("--output text (%q) differs from default (%q)", explicit, bare)
	}
}

// TestMuteListJSON proves the envelope shape and that the JSON facts match the
// text facts for a seeded, still-active mute.
func TestMuteListJSON(t *testing.T) {
	st := seedStore(t)
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	created, err := st.CreateNotifyMute(context.Background(), store.NotifyMute{
		ScopeKind:  store.ScopeHost,
		ScopeValue: "box1",
		Reason:     "planned reboot",
		CreatedAt:  time.Now().UTC(),
		ExpiresAt:  expires,
		CreatedVia: "cli",
	})
	if err != nil {
		t.Fatalf("seed mute: %v", err)
	}

	text, err := run(t, "mute", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "box1") || !strings.Contains(text, "planned reboot") {
		t.Fatalf("text missing seeded facts: %q", text)
	}

	out, err := run(t, "mute", "list", "--output", "json")
	if err != nil {
		t.Fatalf("mute list --output json: %v (%s)", err, out)
	}
	var env struct {
		SchemaVersion int `json:"schema_version"`
		Kind          string
		Data          []struct {
			ID         int64  `json:"id"`
			ScopeKind  string `json:"scope_kind"`
			ScopeValue string `json:"scope_value"`
			Reason     string `json:"reason"`
			CreatedVia string `json:"created_via"`
			ExpiresAt  string `json:"expires_at"`
		}
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if env.SchemaVersion != 1 || env.Kind != "mute.list" {
		t.Fatalf("bad envelope header: %+v", env)
	}
	if len(env.Data) != 1 {
		t.Fatalf("data len = %d, want 1: %s", len(env.Data), out)
	}
	row := env.Data[0]
	if row.ID != created.ID || row.ScopeKind != "host" || row.ScopeValue != "box1" ||
		row.Reason != "planned reboot" || row.CreatedVia != "cli" ||
		row.ExpiresAt != expires.Format(time.RFC3339) {
		t.Fatalf("json facts do not match seeded mute: %+v", row)
	}
}

// TestMuteListJSONEmpty proves the empty case renders "data":[] not null, the
// same discipline every other list projection follows.
func TestMuteListJSONEmpty(t *testing.T) {
	seedStore(t)
	out, err := run(t, "mute", "list", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != `{"schema_version":1,"kind":"mute.list","data":[]}` {
		t.Fatalf("empty mute list json = %q", out)
	}
}
