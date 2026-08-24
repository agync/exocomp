package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestLitestreamConfigWarnsCleartextCreds: a target with embedded userinfo
// (an sftp password) gets the same cleartext-creds warning `backup add-target`
// gives, at the point exocomp renders it — parity for a value stored the same
// unencrypted way.
func TestLitestreamConfigWarnsCleartextCreds(t *testing.T) {
	useDataDir(t, t.TempDir())

	const sentinel = "s3kr1t-p4ss" //nolint:gosec // test sentinel, not a real credential
	if out, err := runLitestream(t, "target", "sftp://replicator:"+sentinel+"@nas.local/db", "--ack-unencrypted"); err != nil {
		t.Fatalf("set target: %v (%s)", err, out)
	}

	cfg, err := runLitestream(t, "config")
	if err != nil {
		t.Fatalf("config: %v (%s)", err, cfg)
	}
	if !strings.Contains(cfg, "WARNING") || !strings.Contains(cfg, "cleartext") {
		t.Errorf("config did not warn about cleartext creds:\n%s", cfg)
	}
	if strings.Contains(cfg, sentinel) {
		t.Errorf("config echoed the credential itself:\n%s", cfg)
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

// TestLitestreamStatusJSONNotConfigured proves the envelope shape and that
// with no target configured the JSON facts match the text facts.
func TestLitestreamStatusJSONNotConfigured(t *testing.T) {
	useDataDir(t, t.TempDir())

	text, err := run(t, "litestream", "status")
	if err != nil {
		t.Fatalf("text status: %v (%s)", err, text)
	}
	if !strings.Contains(text, "none configured") {
		t.Fatalf("text missing not-configured fact: %q", text)
	}

	out, err := run(t, "litestream", "status", "--output", "json")
	if err != nil {
		t.Fatalf("litestream status --output json: %v (%s)", err, out)
	}
	var env struct {
		SchemaVersion int `json:"schema_version"`
		Kind          string
		Data          struct {
			BinaryPresent    bool   `json:"binary_present"`
			TargetConfigured bool   `json:"target_configured"`
			ReplicationState string `json:"replication_state"`
		}
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if env.SchemaVersion != 1 || env.Kind != "litestream.status" {
		t.Fatalf("bad envelope header: %+v", env)
	}
	if env.Data.TargetConfigured || env.Data.ReplicationState != "not_configured" {
		t.Fatalf("json facts do not match not-configured text: %+v", env.Data)
	}
}

// TestLitestreamStatusJSONFreshLocalTarget seeds a local file replica with a
// frame on disk, so the JSON facts land in the "fresh" state with an absolute
// replica_fresh_at — the same event the text "last frame ... ago" line
// reports, relative to now — and never leak the target's raw path.
func TestLitestreamStatusJSONFreshLocalTarget(t *testing.T) {
	useDataDir(t, t.TempDir())

	replica := t.TempDir()
	if err := os.WriteFile(filepath.Join(replica, "generations.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed replica frame: %v", err)
	}
	if out, err := run(t, "litestream", "target", replica); err != nil {
		t.Fatalf("set target: %v (%s)", err, out)
	}

	text, err := run(t, "litestream", "status")
	if err != nil {
		t.Fatalf("text status: %v (%s)", err, text)
	}
	if !strings.Contains(text, "last frame") {
		t.Fatalf("text did not report a fresh frame: %q", text)
	}

	out, err := run(t, "litestream", "status", "--output", "json")
	if err != nil {
		t.Fatalf("litestream status --output json: %v (%s)", err, out)
	}
	var env struct {
		Kind string
		Data struct {
			TargetConfigured bool   `json:"target_configured"`
			TargetType       string `json:"target_type"`
			ReplicationState string `json:"replication_state"`
			ReplicaFreshAt   string `json:"replica_fresh_at"`
		}
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %v (%q)", err, out)
	}
	if !env.Data.TargetConfigured || env.Data.TargetType != "file" ||
		env.Data.ReplicationState != "fresh" || env.Data.ReplicaFreshAt == "" {
		t.Fatalf("json facts do not match a fresh local target: %+v", env.Data)
	}
	if _, err := time.Parse(time.RFC3339, env.Data.ReplicaFreshAt); err != nil {
		t.Fatalf("replica_fresh_at not RFC3339: %v", err)
	}
	if strings.Contains(out, replica) {
		t.Fatalf("litestream status json leaked the target's raw path: %s", out)
	}
}
