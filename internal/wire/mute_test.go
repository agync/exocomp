package wire_test

import (
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func TestGoldenMuteList(t *testing.T) {
	rows := []store.NotifyMute{{
		ID:         5,
		ScopeKind:  store.ScopeHost,
		ScopeValue: "box1",
		Reason:     "planned reboot",
		CreatedVia: "cli",
		ExpiresAt:  at("2026-01-02T03:04:05Z"),
	}}
	got, err := wire.MarshalSuccess(wire.KindMuteList, wire.NewMuteList(rows))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"mute.list","data":[{"id":5,"scope_kind":"host","scope_value":"box1",` +
		`"reason":"planned reboot","created_via":"cli","expires_at":"2026-01-02T03:04:05Z"}]}` + "\n"
	if string(got) != want {
		t.Fatalf("mute list bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

func TestGoldenMuteListEmpty(t *testing.T) {
	got, err := wire.MarshalSuccess(wire.KindMuteList, wire.NewMuteList(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"mute.list","data":[]}` + "\n"
	if string(got) != want {
		t.Fatalf("empty mute list must render data:[] not null:\n got: %s\nwant: %s", got, want)
	}
}

func TestGoldenMuteListGlobal(t *testing.T) {
	rows := []store.NotifyMute{{
		ID:         1,
		ScopeKind:  store.ScopeGlobal,
		CreatedVia: "cli",
		ExpiresAt:  at("2026-01-02T03:04:05Z"),
	}}
	got, err := wire.MarshalSuccess(wire.KindMuteList, wire.NewMuteList(rows))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"mute.list","data":[{"id":1,"scope_kind":"global",` +
		`"created_via":"cli","expires_at":"2026-01-02T03:04:05Z"}]}` + "\n"
	if string(got) != want {
		t.Fatalf("global mute bytes drift:\n got: %s\nwant: %s", got, want)
	}
}
