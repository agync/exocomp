package wire_test

import (
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

func TestGoldenLitestreamStatusFresh(t *testing.T) {
	view := wire.LitestreamStatusView{
		BinaryPresent:    true,
		BinaryVersion:    "litestream v0.3.13",
		TargetConfigured: true,
		TargetType:       "file",
		ReplicationState: wire.LitestreamReplicationFresh,
		ReplicaFreshAt:   "2026-01-02T03:04:05Z",
	}
	got, err := wire.MarshalSuccess(wire.KindLitestreamStatus, view)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"litestream.status","data":{"binary_present":true,` +
		`"binary_version":"litestream v0.3.13","target_configured":true,"target_type":"file",` +
		`"replication_state":"fresh","replica_fresh_at":"2026-01-02T03:04:05Z"}}` + "\n"
	if string(got) != want {
		t.Fatalf("litestream status bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

func TestGoldenLitestreamStatusNotConfigured(t *testing.T) {
	view := wire.LitestreamStatusView{
		ReplicationState: wire.LitestreamReplicationNotConfigured,
	}
	got, err := wire.MarshalSuccess(wire.KindLitestreamStatus, view)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"litestream.status","data":{"binary_present":false,` +
		`"target_configured":false,"replication_state":"not_configured"}}` + "\n"
	if string(got) != want {
		t.Fatalf("litestream not-configured bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestGoldenLitestreamStatusNeverLeaksTargetURI is a structural guard: the
// projection type must expose no field that could carry a target's raw URI
// (an sftp:// target can embed user:pass).
func TestGoldenLitestreamStatusNeverLeaksTargetURI(t *testing.T) {
	view := wire.LitestreamStatusView{
		TargetConfigured: true,
		TargetType:       "sftp",
		TargetRemote:     true,
		ReplicationState: wire.LitestreamReplicationRemoteUnknown,
	}
	got, err := wire.MarshalSuccess(wire.KindLitestreamStatus, view)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"://"} {
		if strings.Contains(string(got), banned) {
			t.Fatalf("litestream status leaked %q: %s", banned, got)
		}
	}
}
