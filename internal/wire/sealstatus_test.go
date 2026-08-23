package wire_test

import (
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

func TestGoldenSealStatusConfigured(t *testing.T) {
	view := wire.SealStatusView{
		TransitFilePresent: true,
		EnvConfigured:      true,
		SealAddress:        "https://bao.lan:8200",
		SealKeyName:        "exocomp",
	}
	got, err := wire.MarshalSuccess(wire.KindSealStatus, view)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"seal.status","data":{"transit_file_present":true,` +
		`"env_configured":true,"seal_address":"https://bao.lan:8200","seal_key_name":"exocomp"}}` + "\n"
	if string(got) != want {
		t.Fatalf("seal status bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

func TestGoldenSealStatusAbsent(t *testing.T) {
	got, err := wire.MarshalSuccess(wire.KindSealStatus, wire.SealStatusView{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"seal.status","data":{"transit_file_present":false,"env_configured":false}}` + "\n"
	if string(got) != want {
		t.Fatalf("seal status absent bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestGoldenSealStatusNeverLeaksToken is a structural guard: the projection
// type must expose no field a token could ever be written into.
func TestGoldenSealStatusNeverLeaksToken(t *testing.T) {
	view := wire.SealStatusView{
		TransitFilePresent: true,
		EnvConfigured:      true,
		SealAddress:        "https://bao.lan:8200",
		SealKeyName:        "exocomp",
	}
	got, err := wire.MarshalSuccess(wire.KindSealStatus, view)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"token", "hvs."} {
		if strings.Contains(strings.ToLower(string(got)), banned) {
			t.Fatalf("seal status leaked a token-shaped field %q: %s", banned, got)
		}
	}
}
