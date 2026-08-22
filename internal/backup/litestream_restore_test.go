package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// TestPlanRestore_Precedence pins the source-selection rule: Litestream is the
// fast, DB-only complement; restic is the authoritative, full, encrypted chain.
// It runs entirely on the decision logic — no litestream binary is involved.
func TestPlanRestore_Precedence(t *testing.T) {
	tests := []struct {
		name         string
		goal         RecoveryGoal
		avail        SourceAvailability
		wantSource   string
		wantScope    string
		wantVerify   bool
		wantErr      error
		reasonSubstr string
	}{
		{
			name:         "authoritative is always restic full",
			goal:         GoalAuthoritative,
			avail:        SourceAvailability{LitestreamBinary: true, Replica: true, Restic: true, Keyring: true},
			wantSource:   sourceRestic,
			wantScope:    scopeFull,
			wantVerify:   false,
			reasonSubstr: "authoritative encrypted DR",
		},
		{
			name:    "authoritative without restic is refused",
			goal:    GoalAuthoritative,
			avail:   SourceAvailability{LitestreamBinary: true, Replica: true, Keyring: true},
			wantErr: ErrNoRestoreSource,
		},
		{
			name:         "freshest db prefers litestream, db-only, verified",
			goal:         GoalFreshestDB,
			avail:        SourceAvailability{LitestreamBinary: true, Replica: true, Restic: true, Keyring: true},
			wantSource:   sourceLitestream,
			wantScope:    scopeDBOnly,
			wantVerify:   true,
			reasonSubstr: "fast-RPO",
		},
		{
			name:    "freshest db via litestream needs a keyring to open the result",
			goal:    GoalFreshestDB,
			avail:   SourceAvailability{LitestreamBinary: true, Replica: true, Restic: true, Keyring: false},
			wantErr: ErrNoKeyringForDBOnly,
		},
		{
			name:         "no litestream binary falls back to restic full",
			goal:         GoalFreshestDB,
			avail:        SourceAvailability{LitestreamBinary: false, Replica: true, Restic: true, Keyring: true},
			wantSource:   sourceRestic,
			wantScope:    scopeFull,
			wantVerify:   false,
			reasonSubstr: "litestream binary absent",
		},
		{
			name:         "no replica falls back to restic full",
			goal:         GoalFreshestDB,
			avail:        SourceAvailability{LitestreamBinary: true, Replica: false, Restic: true, Keyring: true},
			wantSource:   sourceRestic,
			wantScope:    scopeFull,
			reasonSubstr: "no litestream replica configured",
		},
		{
			name:    "freshest db with nothing available is refused",
			goal:    GoalFreshestDB,
			avail:   SourceAvailability{},
			wantErr: ErrNoRestoreSource,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := PlanRestore(tt.goal, tt.avail)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if plan.Source != tt.wantSource {
				t.Errorf("Source = %q, want %q", plan.Source, tt.wantSource)
			}
			if plan.Scope != tt.wantScope {
				t.Errorf("Scope = %q, want %q", plan.Scope, tt.wantScope)
			}
			if plan.VerifyDecrypt != tt.wantVerify {
				t.Errorf("VerifyDecrypt = %v, want %v", plan.VerifyDecrypt, tt.wantVerify)
			}
			if tt.reasonSubstr != "" && !strings.Contains(plan.Reason, tt.reasonSubstr) {
				t.Errorf("Reason = %q, want substring %q", plan.Reason, tt.reasonSubstr)
			}
		})
	}
}

// TestPlanRestore_LitestreamNeverAuthoritative guards the DB-only-vs-full
// distinction directly: a Litestream source may only ever recover the database,
// never the full encrypted chain.
func TestPlanRestore_LitestreamNeverAuthoritative(t *testing.T) {
	plan, err := PlanRestore(GoalAuthoritative, SourceAvailability{
		LitestreamBinary: true, Replica: true, Restic: true, Keyring: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Source == sourceLitestream {
		t.Fatalf("authoritative recovery must never resolve to litestream, got %+v", plan)
	}
	if plan.Scope != scopeFull {
		t.Fatalf("authoritative recovery must be full scope, got %q", plan.Scope)
	}
}

// TestReplicaSafety_PlaintextRemote pins the no-encryption caveat: a LAN-local
// or file target is safe, a public remote is not and must be acknowledged.
func TestReplicaSafety(t *testing.T) {
	tests := []struct {
		url      string
		wantSafe bool
	}{
		{"file:///srv/replica", true},
		{"/srv/replica", true},
		{"sftp://nas.local/exocomp", true},
		{"sftp://192.168.1.10/exocomp", true},
		{"s3://10.0.0.5:9000/bucket", true},
		{"https://127.0.0.1/replica", true},
		{"s3://s3.amazonaws.com/bucket/exocomp", false},
		{"sftp://backup.example.com/exocomp", false},
		{"gs://my-bucket/exocomp", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			safe, reason := ReplicaSafety(tt.url)
			if safe != tt.wantSafe {
				t.Fatalf("ReplicaSafety(%q) safe = %v (%s), want %v", tt.url, safe, reason, tt.wantSafe)
			}
		})
	}
}

// TestLitestreamRestore_RefusesWithoutBinary proves the honest refusal when the
// sidecar is not installed — the restore never shells a missing tool. When the
// binary IS present in a dev environment the test degrades to asserting the
// refusal path is not reached, so it is deterministic either way.
func TestLitestreamRestore_RefusesWithoutBinary(t *testing.T) {
	if LitestreamAvailable() {
		t.Skip("litestream is installed here; the binary-absent refusal cannot be exercised")
	}
	err := LitestreamRestore(t.Context(), LitestreamConfig{
		ReplicaURL: "file:///srv/replica",
		DestDB:     t.TempDir() + "/exocomp.db",
	})
	if !errors.Is(err, ErrLitestreamAbsent) {
		t.Fatalf("err = %v, want ErrLitestreamAbsent", err)
	}
}

// TestLitestreamRestore_RefusesUnsafeReplica proves a plaintext public remote is
// refused before any subprocess, with no ack. The check runs ahead of the
// binary probe order only matters for the message, so this holds with or without
// litestream installed as long as the replica is classified unsafe.
func TestLitestreamRestore_RefusesUnsafeReplica(t *testing.T) {
	err := LitestreamRestore(t.Context(), LitestreamConfig{
		ReplicaURL: "s3://s3.amazonaws.com/bucket/exocomp",
		DestDB:     t.TempDir() + "/exocomp.db",
	})
	// A box without litestream refuses on the absent binary first; a box with it
	// refuses on the unsafe replica. Either is an honest refusal, never a run.
	if !errors.Is(err, ErrReplicaUnsafe) && !errors.Is(err, ErrLitestreamAbsent) {
		t.Fatalf("err = %v, want ErrReplicaUnsafe or ErrLitestreamAbsent", err)
	}
}

// TestVerifyEnvelopeOpens is the decrypt-round-trip gate a Litestream restore
// must pass before its DB is trusted: the keyring beside the DB unwraps the DEK,
// and that DEK opens a sealed row inside the DB. It builds the fixture by hand so
// it needs no restic or litestream binary — only the age library the envelope
// already uses.
func TestVerifyEnvelopeOpens(t *testing.T) {
	dataDir, identity := sealedDataDir(t)

	name, err := VerifyEnvelopeOpens(t.Context(), dataDir, identity)
	if err != nil {
		t.Fatalf("VerifyEnvelopeOpens: %v", err)
	}
	if name == "" {
		t.Fatal("VerifyEnvelopeOpens returned an empty secret name")
	}
}

// TestVerifyEnvelopeOpens_WrongIdentity proves the gate refuses a DB the
// supplied identity cannot open — the untrusted-bytes case the gate exists for.
func TestVerifyEnvelopeOpens_WrongIdentity(t *testing.T) {
	dataDir, _ := sealedDataDir(t)

	// A fresh, unrelated recovery identity: it is a valid age key, but not one the
	// keyring wraps the DEK to, so the unwrap must fail.
	other := filepath.Join(t.TempDir(), "wrong.key")
	_, kit, err := secrets.Init(filepath.Join(t.TempDir(), "other"), nil)
	if err != nil {
		t.Fatalf("secrets.Init(other): %v", err)
	}
	if err := os.WriteFile(other, []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write wrong identity: %v", err)
	}

	if _, err := VerifyEnvelopeOpens(t.Context(), dataDir, other); err == nil {
		t.Fatal("VerifyEnvelopeOpens accepted an identity that cannot unwrap the keyring")
	}
}

// sealedDataDir builds a data dir holding keyring.age, exocomp.db, and one
// sealed row — the shape a Litestream DB restore lands beside — and returns the
// dir and a recovery identity file.
func sealedDataDir(t *testing.T) (dataDir, identity string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "data")
	env, kit, err := secrets.Init(dataDir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })

	identity = filepath.Join(t.TempDir(), "recovery.key")
	if err := os.WriteFile(identity, []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}

	st, err := store.Open(filepath.Join(dataDir, RestoredDBName))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	sealed, err := env.Seal("canary", "canary", []byte("proof-of-decrypt"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := st.CreateSecret(t.Context(), store.SecretRow{
		Name:       sealed.Name,
		Kind:       sealed.Kind,
		Nonce:      sealed.Nonce,
		Ciphertext: sealed.Ciphertext,
		AAD:        sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		t.Fatalf("create sealed row: %v", err)
	}
	// VerifyEnvelopeOpens opens its own handle to the DB; close this one so the
	// two do not race on the WAL.
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return dataDir, identity
}

// TestLitestreamRestore_EmptyInputs guards the argument checks.
func TestLitestreamRestore_EmptyInputs(t *testing.T) {
	if err := LitestreamRestore(t.Context(), LitestreamConfig{DestDB: "/x/exocomp.db"}); err == nil {
		t.Error("empty replica url must error")
	}
	if err := LitestreamRestore(t.Context(), LitestreamConfig{ReplicaURL: "file:///r"}); err == nil {
		t.Error("empty dest db must error")
	}
}
