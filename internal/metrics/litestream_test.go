package metrics

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/litestream"
)

// TestLitestreamGaugeUnconfigured: with no replica target set, freshness is
// honest "unknown" (+Inf) and configured is 0 — never a misleading zero age.
func TestLitestreamGaugeUnconfigured(t *testing.T) {
	st := testStore(t)
	r := New(st, "")
	out, err := r.Render(context.Background())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, `exocomp_litestream_replica_configured 0`) {
		t.Errorf("want replica_configured 0, got:\n%s", out)
	}
	if !strings.Contains(out, `exocomp_litestream_replication_staleness_seconds +Inf`) {
		t.Errorf("want staleness +Inf, got:\n%s", out)
	}
}

// TestLitestreamGaugeFileReplica: a configured file replica with a frame yields
// a real staleness age and a last-frame timestamp.
func TestLitestreamGaugeFileReplica(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	replicaDir := t.TempDir()
	frame := filepath.Join(replicaDir, "0000000000000001.wal.lz4")
	if err := os.WriteFile(frame, []byte("x"), 0o644); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	mtime := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	if err := os.Chtimes(frame, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := st.PutSetting(ctx, litestream.SettingReplicaTarget, replicaDir); err != nil {
		t.Fatalf("put setting: %v", err)
	}

	r := New(st, "")
	atClock(r, mtime.Add(45*time.Second))
	out, err := r.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, `exocomp_litestream_replica_configured 1`) {
		t.Errorf("want replica_configured 1, got:\n%s", out)
	}
	if !strings.Contains(out, `exocomp_litestream_replication_staleness_seconds 45`) {
		t.Errorf("want staleness 45, got:\n%s", out)
	}
}

// TestLitestreamGaugeRemoteUnknown: a configured remote replica is "configured"
// but its freshness is unknown to exocomp, so staleness stays +Inf.
func TestLitestreamGaugeRemoteUnknown(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.PutSetting(ctx, litestream.SettingReplicaTarget, "s3://bucket/db"); err != nil {
		t.Fatalf("put setting: %v", err)
	}
	r := New(st, "")
	out, err := r.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, `exocomp_litestream_replica_configured 1`) {
		t.Errorf("want replica_configured 1, got:\n%s", out)
	}
	if !strings.Contains(out, `exocomp_litestream_replication_staleness_seconds +Inf`) {
		t.Errorf("want staleness +Inf for remote, got:\n%s", out)
	}
}
