package litestream

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const goldenDBPath = "/home/op/.local/share/exocomp/exocomp.db"

// TestGenerateConfigGolden pins the exact bytes emitted for a local and an s3
// target. Generation is the deterministic seam the sidecar depends on, so a
// drift here is a real regression, not a formatting nit.
func TestGenerateConfigGolden(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		ack    bool
		golden string
	}{
		{
			name:   "local file replica",
			raw:    "/mnt/nas/exocomp-replica",
			golden: "local.yml",
		},
		{
			name:   "s3 replica, acknowledged",
			raw:    "s3://exocomp-backups/db?region=us-east-1&endpoint=https://s3.example.com",
			ack:    true,
			golden: "s3.yml",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ParseTarget(tc.raw)
			if err != nil {
				t.Fatalf("parse target: %v", err)
			}
			got, err := GenerateConfig(Config{DBPath: goldenDBPath, Target: target, AckUnencrypted: tc.ack})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", tc.golden))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if got != string(want) {
				t.Errorf("config mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", tc.golden, got, want)
			}
		})
	}
}

// TestGenerateConfigUnencryptedRefusal proves the caveat is enforced both ways:
// a remote target without an ack is refused with ErrUnencryptedRemote, and the
// same target with the ack renders.
func TestGenerateConfigUnencryptedRefusal(t *testing.T) {
	target, err := ParseTarget("s3://exocomp-backups/db")
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}

	if _, err := GenerateConfig(Config{DBPath: goldenDBPath, Target: target}); !errors.Is(err, ErrUnencryptedRemote) {
		t.Fatalf("unacknowledged remote target: want ErrUnencryptedRemote, got %v", err)
	}

	out, err := GenerateConfig(Config{DBPath: goldenDBPath, Target: target, AckUnencrypted: true})
	if err != nil {
		t.Fatalf("acknowledged remote target: unexpected error %v", err)
	}
	if out == "" {
		t.Fatal("acknowledged remote target produced empty config")
	}
}

// TestGenerateConfigLocalNeedsNoAck confirms a LAN-local file replica renders
// without an acknowledgement — the ack gates remote targets only.
func TestGenerateConfigLocalNeedsNoAck(t *testing.T) {
	target, err := ParseTarget("/srv/replica")
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	if _, err := GenerateConfig(Config{DBPath: goldenDBPath, Target: target}); err != nil {
		t.Fatalf("local target without ack: unexpected error %v", err)
	}
}

// TestGenerateConfigRejectsEmptyInputs guards the two nil-ish inputs a caller
// could pass by mistake.
func TestGenerateConfigRejectsEmptyInputs(t *testing.T) {
	if _, err := GenerateConfig(Config{DBPath: "", Target: Target{Type: ReplicaFile, Path: "/x"}}); err == nil {
		t.Error("empty db path: want error, got nil")
	}
	if _, err := GenerateConfig(Config{DBPath: goldenDBPath, Target: Target{}}); err == nil {
		t.Error("unparsed target: want error, got nil")
	}
}
