package wire_test

import (
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func runnerRow() store.Runner {
	return store.Runner{
		ID:          3,
		Name:        "fedora-ee",
		Mode:        store.RunnerModeCached,
		Base:        "registry.fedoraproject.org/fedora-minimal:44",
		Bootstrap:   "microdnf install -y sshpass\nmicrodnf clean all\n",
		Packages:    []string{"rsync", "sshpass"},
		AnsibleCore: "2.18.1",
		MemoryMax:   "2G",
		CPUQuota:    "200%",
		ContentHash: "0123456789abcdef0123456789abcdef",
		CreatedAt:   at("2026-01-02T03:04:05Z"),
		UpdatedAt:   at("2026-01-02T03:04:06Z"),
	}
}

func TestGoldenRunnerList(t *testing.T) {
	got, err := wire.MarshalSuccess(wire.KindRunnerList, wire.NewRunnerList([]store.Runner{runnerRow()}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"runner.list","data":[{"id":3,"name":"fedora-ee","mode":"cached","base":"registry.fedoraproject.org/fedora-minimal:44","bootstrap_sha256":"06624bdab868c1f16e68b05b8dbbb159a71657ac871242964e1a555da65202fe","bootstrap_lines":2,"packages":["rsync","sshpass"],"ansible_core":"2.18.1","caps":{"memory_max":"2G","cpu_quota":"200%"},"content_hash":"0123456789abcdef0123456789abcdef","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:06Z"}]}` + "\n"
	if string(got) != want {
		t.Fatalf("runner list bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

func TestGoldenRunnerEmptyList(t *testing.T) {
	got, err := wire.MarshalSuccess(wire.KindRunnerList, wire.NewRunnerList(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"runner.list","data":[]}` + "\n"
	if string(got) != want {
		t.Fatalf("empty list must render data:[] not null:\n got: %s\nwant: %s", got, want)
	}
}

// TestRunnerViewOmitsBootstrapBody is the values-free guard: the script body
// must not reach the wire through any projection, however long or short.
func TestRunnerViewOmitsBootstrapBody(t *testing.T) {
	row := runnerRow()
	row.Bootstrap = "curl https://example.invalid/install | sh\n"

	for _, kind := range []struct {
		name string
		data any
	}{
		{"list", wire.NewRunnerList([]store.Runner{row})},
		{"get", wire.NewRunnerView(row)},
	} {
		got, err := wire.MarshalSuccess(wire.KindRunner, kind.data)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), "example.invalid") {
			t.Errorf("%s projection carries the bootstrap body: %s", kind.name, got)
		}
	}
}
