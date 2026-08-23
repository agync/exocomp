package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// store is a stand-in for the sealed-secret rows the real store holds during a
// DEK rotation. reseal walks it exactly as the store-backed reconciler does.
type store struct {
	rows  map[string]SealedSecret
	order []string
}

func newStore(t *testing.T, env *Envelope, m int) (*store, map[string]string) {
	t.Helper()
	s := &store{rows: map[string]SealedSecret{}}
	want := map[string]string{}
	for i := 0; i < m; i++ {
		name := fmt.Sprintf("s%d", i)
		pt := fmt.Sprintf("plaintext-%d", i)
		sealed, err := env.Seal(name, "generic", []byte(pt))
		if err != nil {
			t.Fatalf("seal %s: %v", name, err)
		}
		s.rows[name] = sealed
		s.order = append(s.order, name)
		want[name] = pt
	}
	return s, want
}

// openEither tries the new DEK first (a row already moved), then the old (a row
// not yet moved). newEnv is always present during a rotation; oldEnv may be nil
// once the old keyring no longer opens, by which point every row is already on
// the new DEK.
func openEither(oldEnv, newEnv *Envelope, s SealedSecret) ([]byte, error) {
	if newEnv != nil {
		if pt, err := newEnv.OpenSecret(s); err == nil {
			return pt, nil
		}
	}
	if oldEnv != nil {
		if pt, err := oldEnv.OpenSecret(s); err == nil {
			return pt, nil
		}
	}
	return nil, fmt.Errorf("secrets: row %q opens under neither DEK", s.Name)
}

// resealFrom re-seals rows under the new DEK. It stops with an injected error
// once it has moved stopAfter rows (stopAfter < 0 moves them all), standing in
// for a crash mid-loop. Because it opens each row under whichever DEK holds it,
// the same function drives both the forward reseal and the reconciliation.
func (s *store) resealFrom(stopAfter int) func(oldEnv, newEnv *Envelope) error {
	return func(oldEnv, newEnv *Envelope) error {
		for i, name := range s.order {
			if stopAfter >= 0 && i >= stopAfter {
				return fmt.Errorf("injected crash after %d of %d rows", stopAfter, len(s.order))
			}
			pt, err := openEither(oldEnv, newEnv, s.rows[name])
			if err != nil {
				return err
			}
			ns, err := newEnv.Seal(s.rows[name].Name, s.rows[name].Kind, pt)
			clear(pt)
			if err != nil {
				return err
			}
			s.rows[name] = ns
		}
		return nil
	}
}

func (s *store) assertAllOpen(t *testing.T, env *Envelope, want map[string]string) {
	t.Helper()
	if len(s.rows) != len(want) {
		t.Fatalf("row count = %d, want %d: a row was lost", len(s.rows), len(want))
	}
	for name, wantPT := range want {
		got, err := env.OpenSecret(s.rows[name])
		if err != nil {
			t.Errorf("row %q does not open under the live DEK: %v", name, err)
			continue
		}
		if string(got) != wantPT {
			t.Errorf("row %q opened as %q, want %q", name, got, wantPT)
		}
	}
}

// TestResealCrashReconcilesToCompletion is the finding: a crash after N of M
// rows must be recoverable on the next Open. The rotation is interrupted
// mid-loop, then reconciled; every row must decrypt under the NEW DEK,
// keyring.age must be the new keyring, keyring.age.next must be gone, and no row
// may be lost.
func TestResealCrashReconcilesToCompletion(t *testing.T) {
	dir := t.TempDir()
	victim, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate victim: %v", err)
	}
	env, _ := mustInit(t, dir, []string{victim.Recipient().String()})

	const m = 6
	s, want := newStore(t, env, m)
	_ = env.Close()

	// Interrupt the rotation after 3 of 6 rows.
	if _, err := RotateDEK(dir, MigrateOptions{Drop: []string{"extra-1"}}, s.resealFrom(3)); err == nil {
		t.Fatal("RotateDEK returned nil despite the injected crash")
	}
	if !ResealPending(dir) {
		t.Fatal("no staged keyring after an interrupted rotation")
	}

	// A plain Open here would strand the moved rows: keyring.age still wraps the
	// old DEK. Reconcile is what makes the next Open whole.
	done, err := Reconcile(dir, s.resealFrom(-1))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !done {
		t.Fatal("Reconcile reported nothing to do on a pending rotation")
	}
	if ResealPending(dir) {
		t.Fatal("keyring.age.next survived reconciliation")
	}
	if _, err := os.Stat(filepath.Join(dir, keyringNextName)); !os.IsNotExist(err) {
		t.Fatalf("staged keyring still on disk: %v", err)
	}

	live, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after reconcile: %v", err)
	}
	defer func() { _ = live.Close() }()
	if live.KeyringVersion().Version != 2 {
		t.Errorf("keyring version = %s, want v2", live.KeyringVersion())
	}
	s.assertAllOpen(t, live, want)

	// Revocation actually completed: the dropped recipient's retained keyring no
	// longer unwraps the live keyring.
	if _, err := unwrapWith(t, dir, victim); err == nil {
		t.Fatal("dropped recipient still unwraps the reconciled keyring")
	}
}

// TestResealCleanRunNeedsNoReconcile guards the happy path: a rotation that does
// not crash finishes on its own, leaves no staged files, and reconciling
// afterward is a no-op.
func TestResealCleanRunNeedsNoReconcile(t *testing.T) {
	dir := t.TempDir()
	victim, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate victim: %v", err)
	}
	env, _ := mustInit(t, dir, []string{victim.Recipient().String()})
	const m = 4
	s, want := newStore(t, env, m)
	_ = env.Close()

	if _, err := RotateDEK(dir, MigrateOptions{Drop: []string{"extra-1"}}, s.resealFrom(-1)); err != nil {
		t.Fatalf("RotateDEK: %v", err)
	}
	if ResealPending(dir) {
		t.Fatal("a clean rotation left a staged keyring behind")
	}
	for _, name := range []string{recipientsNextName, keyringVersionNextName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("clean rotation left %s behind", name)
		}
	}
	done, err := Reconcile(dir, s.resealFrom(-1))
	if err != nil {
		t.Fatalf("Reconcile after clean run: %v", err)
	}
	if done {
		t.Fatal("Reconcile claimed work on a store with nothing pending")
	}

	live, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = live.Close() }()
	s.assertAllOpen(t, live, want)
}

// TestResealCrashWithHostRotationRecovers exercises the staged host key: a
// rotation that both drops a recipient AND rotates the host identity crashes
// mid-reseal, then reconciles. The new host key must open every row and the old
// host key must open nothing, with no row lost.
func TestResealCrashWithHostRotationRecovers(t *testing.T) {
	dir := t.TempDir()
	victim, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate victim: %v", err)
	}
	env, _ := mustInit(t, dir, []string{victim.Recipient().String()})

	oldHostRaw, err := os.ReadFile(filepath.Join(dir, hostKeyFile))
	if err != nil {
		t.Fatalf("read host key: %v", err)
	}
	oldHost, err := age.ParseX25519Identity(strings.TrimSpace(string(oldHostRaw)))
	if err != nil {
		t.Fatalf("parse host key: %v", err)
	}

	const m = 5
	s, want := newStore(t, env, m)
	_ = env.Close()

	if _, err := RotateDEK(dir, MigrateOptions{Drop: []string{"extra-1"}, RotateHost: true}, s.resealFrom(2)); err == nil {
		t.Fatal("RotateDEK returned nil despite the injected crash")
	}
	if !ResealPending(dir) {
		t.Fatal("no staged keyring after an interrupted host-rotating rotation")
	}

	done, err := Reconcile(dir, s.resealFrom(-1))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !done || ResealPending(dir) {
		t.Fatalf("host-rotating rotation did not reconcile: done=%v pending=%v", done, ResealPending(dir))
	}

	live, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after reconcile: %v", err)
	}
	defer func() { _ = live.Close() }()
	s.assertAllOpen(t, live, want)

	// The host identity actually rotated: the old host key no longer unwraps.
	if _, err := unwrapWith(t, dir, oldHost); err == nil {
		t.Fatal("the replaced host identity still unwraps the reconciled keyring")
	}
	if _, err := os.Stat(filepath.Join(dir, hostKeyNextName)); !os.IsNotExist(err) {
		t.Fatalf("staged host key still on disk: %v", err)
	}
}

// TestResealDoubleCrashConverges injects a second crash during reconciliation
// itself. The next Reconcile must still converge, not compound the damage.
func TestResealDoubleCrashConverges(t *testing.T) {
	dir := t.TempDir()
	victim, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate victim: %v", err)
	}
	env, _ := mustInit(t, dir, []string{victim.Recipient().String()})
	const m = 6
	s, want := newStore(t, env, m)
	_ = env.Close()

	// First crash: rotation interrupted after 2 rows.
	if _, err := RotateDEK(dir, MigrateOptions{Drop: []string{"extra-1"}}, s.resealFrom(2)); err == nil {
		t.Fatal("RotateDEK returned nil despite the injected crash")
	}
	// Second crash: reconciliation interrupted after 4 rows. It must fail without
	// promoting, leaving the staged keyring in place for the next attempt.
	if _, err := Reconcile(dir, s.resealFrom(4)); err == nil {
		t.Fatal("Reconcile returned nil despite the second injected crash")
	}
	if !ResealPending(dir) {
		t.Fatal("a failed reconciliation promoted the keyring anyway")
	}
	// Third attempt runs to completion.
	done, err := Reconcile(dir, s.resealFrom(-1))
	if err != nil {
		t.Fatalf("Reconcile (converging): %v", err)
	}
	if !done || ResealPending(dir) {
		t.Fatalf("reconciliation did not converge: done=%v pending=%v", done, ResealPending(dir))
	}

	live, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after convergence: %v", err)
	}
	defer func() { _ = live.Close() }()
	s.assertAllOpen(t, live, want)
	if _, err := unwrapWith(t, dir, victim); err == nil {
		t.Fatal("dropped recipient still unwraps after convergence")
	}
}
