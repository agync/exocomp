package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
)

// TestServeReconcilesInterruptedRotation proves the real store-backed recovery
// path: a DEK rotation crashes mid-loop over actual database rows, and the
// daemon's startup reconciliation (storeReseal + secrets.Reconcile) finishes it
// so every sealed row opens under the new DEK. This is the end-to-end twin of
// internal/secrets' TestResealCrashReconcilesToCompletion.
func TestServeReconcilesInterruptedRotation(t *testing.T) {
	dir, _ := initForKit(t)
	ctx := context.Background()

	const m = 6
	want := map[string]string{}
	for i := 0; i < m; i++ {
		name := fmt.Sprintf("s%d", i)
		val := fmt.Sprintf("value-%d", i)
		sealSecretForTest(t, dir, name, val)
		want[name] = val
	}

	st, err := openStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// A crash after 3 of 6 rows: move the first three through the store, then fail.
	crashAfter := 3
	moved := 0
	realReseal := storeReseal(ctx, st)
	crashing := func(oldEnv, newEnv *secrets.Envelope) error {
		rows, err := st.ListSecrets(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if moved >= crashAfter {
				return fmt.Errorf("injected crash after %d rows", crashAfter)
			}
			sealed := secrets.SealedSecret{Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext}
			pt, err := oldEnv.OpenSecret(sealed)
			if err != nil {
				return err
			}
			ns, err := newEnv.Seal(row.Name, row.Kind, pt)
			clear(pt)
			if err != nil {
				return err
			}
			if err := st.RotateSecret(ctx, row.Name, ns.Nonce, ns.Ciphertext); err != nil {
				return err
			}
			moved++
		}
		return nil
	}

	if _, err := secrets.RotateDEK(dir, secrets.MigrateOptions{RotateHost: false}, crashing); err == nil {
		t.Fatal("RotateDEK returned nil despite the injected crash")
	}
	if !secrets.ResealPending(dir) {
		t.Fatal("no staged keyring after an interrupted rotation")
	}

	// The interrupted store is split: some rows moved to the new DEK, the rest not,
	// and keyring.age still wraps the OLD DEK. A plain Open cannot read the moved
	// rows.
	old, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("Open (old DEK still active): %v", err)
	}
	stranded := 0
	for name := range want {
		row, err := st.GetSecret(ctx, name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if _, err := old.OpenSecret(secrets.SealedSecret{
			Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
		}); err != nil {
			stranded++
		}
	}
	_ = old.Close()
	if stranded == 0 {
		t.Fatal("expected some rows stranded under the new DEK before reconciliation")
	}

	// Daemon-startup recovery: the same call serve makes.
	reconciled, err := secrets.Reconcile(dir, realReseal)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !reconciled {
		t.Fatal("Reconcile did nothing on a pending rotation")
	}
	if secrets.ResealPending(dir) {
		t.Fatal("keyring.age.next survived reconciliation")
	}

	live, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("Open after reconcile: %v", err)
	}
	defer func() { _ = live.Close() }()
	for name, wantVal := range want {
		row, err := st.GetSecret(ctx, name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		pt, err := live.OpenSecret(secrets.SealedSecret{
			Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
		})
		if err != nil {
			t.Errorf("row %q does not open under the new DEK: %v", name, err)
			continue
		}
		if string(pt) != wantVal {
			t.Errorf("row %q = %q, want %q", name, pt, wantVal)
		}
	}

	// keyring.age.next is gone and keyring.age is the live keyring the reconciled
	// Open just used.
	if _, err := os.Stat(filepath.Join(dir, secrets.KeyringName+".next")); !os.IsNotExist(err) {
		t.Fatalf("staged keyring still on disk: %v", err)
	}
}
