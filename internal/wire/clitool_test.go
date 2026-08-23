package wire_test

import (
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func cliToolRow() store.CLITool {
	return store.CLITool{
		ID:          4,
		Name:        "kubectl",
		Binary:      "/usr/bin/kubectl",
		FixedArgs:   []string{"--context", "homelab"},
		ArgPattern:  "[a-z0-9./=-]+",
		Egress:      store.CLIEgressHTTPS,
		Description: "cluster reads and rollouts",
		CreatedAt:   at("2026-01-02T03:04:05Z"),
		UpdatedAt:   at("2026-01-02T03:04:06Z"),
	}
}

func TestGoldenCLIToolList(t *testing.T) {
	got, err := wire.MarshalSuccess(wire.KindCLIToolList, wire.NewCLIToolList([]store.CLITool{cliToolRow()}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"cli-tool.list","data":[{"id":4,"name":"kubectl","binary":"/usr/bin/kubectl","fixed_args":["--context","homelab"],"arg_pattern":"[a-z0-9./=-]+","egress":"https","description":"cluster reads and rollouts","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:06Z"}]}` + "\n"
	if string(got) != want {
		t.Fatalf("cli tool list bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

func TestGoldenCLIToolEmptyList(t *testing.T) {
	got, err := wire.MarshalSuccess(wire.KindCLIToolList, wire.NewCLIToolList(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"cli-tool.list","data":[]}` + "\n"
	if string(got) != want {
		t.Fatalf("empty cli tool list drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestCLIToolViewCarriesNoSecret: an allow-list entry holds no credential, and
// the projection must not grow a field where one could arrive later.
func TestCLIToolViewCarriesNoSecret(t *testing.T) {
	got, err := wire.MarshalSuccess(wire.KindCLITool, wire.NewCLIToolView(cliToolRow()))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "secret", "password", "key"} {
		if strings.Contains(strings.ToLower(string(got)), forbidden) {
			t.Errorf("the cli tool projection carries a %q field: %s", forbidden, got)
		}
	}
}
