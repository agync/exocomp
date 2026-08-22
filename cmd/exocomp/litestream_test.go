package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/litestream"
)

// runLitestream executes the litestream command tree with args and returns its
// combined output and error, the way an operator would invoke it.
func runLitestream(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newLitestreamCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// TestLitestreamTargetRefusesUnencryptedRemote: a remote target without the ack
// is refused at persistence, and nothing is stored.
func TestLitestreamTargetRefusesUnencryptedRemote(t *testing.T) {
	useDataDir(t, t.TempDir())

	_, err := runLitestream(t, "target", "s3://bucket/db")
	if !errors.Is(err, litestream.ErrUnencryptedRemote) {
		t.Fatalf("want ErrUnencryptedRemote, got %v", err)
	}

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if v, err := st.GetSetting(context.Background(), litestream.SettingReplicaTarget); err == nil {
		t.Errorf("refused target was persisted anyway: %q", v)
	}
}

// TestLitestreamTargetAckThenConfig: the --ack override persists the remote
// target, and `config` then renders it with the no-encryption warning.
func TestLitestreamTargetAckThenConfig(t *testing.T) {
	useDataDir(t, t.TempDir())

	out, err := runLitestream(t, "target", "s3://bucket/db?region=us-east-1", "--ack-unencrypted")
	if err != nil {
		t.Fatalf("acked target: %v (%s)", err, out)
	}

	cfg, err := runLitestream(t, "config")
	if err != nil {
		t.Fatalf("config: %v (%s)", err, cfg)
	}
	for _, want := range []string{"type: s3", "bucket: bucket", "WARNING", "does NOT encrypt", "LITESTREAM_ACCESS_KEY_ID"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q:\n%s", want, cfg)
		}
	}
}

// TestLitestreamTargetLocalNoAck: a LAN-local file replica needs no ack and
// renders a file replica block.
func TestLitestreamTargetLocalNoAck(t *testing.T) {
	useDataDir(t, t.TempDir())

	if out, err := runLitestream(t, "target", "/mnt/nas/exocomp-replica"); err != nil {
		t.Fatalf("local target: %v (%s)", err, out)
	}
	cfg, err := runLitestream(t, "config")
	if err != nil {
		t.Fatalf("config: %v (%s)", err, cfg)
	}
	if !strings.Contains(cfg, "type: file") || !strings.Contains(cfg, "path: /mnt/nas/exocomp-replica") {
		t.Errorf("config missing file replica block:\n%s", cfg)
	}
}

// TestLitestreamConfigNoTarget: config refuses cleanly, naming the fix, when no
// target is configured.
func TestLitestreamConfigNoTarget(t *testing.T) {
	useDataDir(t, t.TempDir())
	out, err := runLitestream(t, "config")
	if err == nil {
		t.Fatalf("want error with no target, got output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no replica target configured") {
		t.Errorf("error did not name the fix: %v", err)
	}
}

// TestLitestreamStatusHonestWithoutBinary: status reports the sidecar state and
// never errors for want of the binary. When the binary is genuinely absent (the
// default on this host), it names the install step.
func TestLitestreamStatusHonestWithoutBinary(t *testing.T) {
	useDataDir(t, t.TempDir())
	if _, err := runLitestream(t, "target", "/srv/replica"); err != nil {
		t.Fatalf("set target: %v", err)
	}

	out, err := runLitestream(t, "status")
	if err != nil {
		t.Fatalf("status errored: %v (%s)", err, out)
	}
	if !strings.Contains(out, "target:") || !strings.Contains(out, "replication:") {
		t.Errorf("status missing expected lines:\n%s", out)
	}
	if binPath, _ := litestream.Presence(); binPath == "" {
		if !strings.Contains(out, "not installed") {
			t.Errorf("binary absent but status did not say so:\n%s", out)
		}
		if !strings.Contains(out, litestream.InstallHint) {
			t.Errorf("status did not name the install step:\n%s", out)
		}
	}
}
