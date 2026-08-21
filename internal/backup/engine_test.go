package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// canaryValue is the secret sealed before every backup in these tests. Reading
// it back after a restore is what proves the whole chain, not just the files.
const canaryValue = "canary-plaintext-9f2c"

type fixture struct {
	dataDir  string
	identity string
	env      *secrets.Envelope
	store    *store.Store
	engine   *Engine
	cache    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireTool(t, "restic")
	requireTool(t, "age")

	dataDir := filepath.Join(t.TempDir(), "data")
	env, kit, err := secrets.Init(dataDir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })

	identity := filepath.Join(t.TempDir(), "recovery.key")
	if err := os.WriteFile(identity, []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write recovery identity: %v", err)
	}

	st, err := store.Open(filepath.Join(dataDir, "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sealed, err := env.Seal("canary", "canary", []byte(canaryValue))
	if err != nil {
		t.Fatalf("seal canary: %v", err)
	}
	_, err = st.CreateSecret(t.Context(), store.SecretRow{
		Name:       sealed.Name,
		Kind:       sealed.Kind,
		Nonce:      sealed.Nonce,
		Ciphertext: sealed.Ciphertext,
		AAD:        sealed.Name + "|" + sealed.Kind,
	})
	if err != nil {
		t.Fatalf("create canary row: %v", err)
	}

	return &fixture{
		dataDir:  dataDir,
		identity: identity,
		env:      env,
		store:    st,
		engine:   New(st, env, Options{}),
		cache:    t.TempDir(),
	}
}

// target registers a target and returns it with a private restic cache, so a
// test never writes into the operator's own cache dir.
func (f *fixture) target(t *testing.T, name, kind, uri string) Target {
	t.Helper()
	row, err := f.store.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: name, Kind: kind, URI: uri, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target %s: %v", name, err)
	}
	return Target{BackupTarget: row, Env: map[string]string{"RESTIC_CACHE_DIR": f.cache}}
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("%s is required for this test and is not on PATH", name)
	}
}

func TestStagingManifestVerifies(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	stage := t.TempDir()
	manifest, err := f.engine.stage(t.Context(), stage)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	if got := manifest.SchemaVersion; got < 1 {
		t.Errorf("schema version = %d, want >= 1", got)
	}
	if manifest.ToolVersions["restic"] == "" || manifest.ToolVersions["restic"] == "missing" {
		t.Errorf("restic tool version = %q", manifest.ToolVersions["restic"])
	}
	if manifest.CreatedAt.IsZero() {
		t.Error("manifest has no timestamp")
	}

	names := make([]string, 0, len(manifest.Files))
	for _, f := range manifest.Files {
		names = append(names, f.Name)
	}
	want := []string{KeyringName, SnapshotName}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("manifest files = %v, want %v", names, want)
	}

	if err := manifest.Verify(stage); err != nil {
		t.Fatalf("verify fresh staging tree: %v", err)
	}

	// The snapshot is a plain SQLite file: opening it must find the canary row.
	snap, err := store.Open(filepath.Join(stage, SnapshotName))
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = snap.Close() }()
	if _, err := snap.GetSecret(t.Context(), "canary"); err != nil {
		t.Fatalf("canary row missing from snapshot: %v", err)
	}

	// A single flipped byte must fail verification.
	body, err := os.ReadFile(filepath.Join(stage, KeyringName))
	if err != nil {
		t.Fatalf("read keyring: %v", err)
	}
	body[len(body)/2] ^= 0xff
	if err := os.WriteFile(filepath.Join(stage, KeyringName), body, 0o600); err != nil {
		t.Fatalf("rewrite keyring: %v", err)
	}
	if err := manifest.Verify(stage); err == nil {
		t.Fatal("verify accepted a tampered staging tree")
	}
}

func TestResticLocalRoundTrip(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	repo := filepath.Join(t.TempDir(), "nas")
	target := f.target(t, "nas", KindResticLocal, repo)

	// Two runs: the second proves the repository is adopted, not re-initialized,
	// and that retention runs against an existing snapshot.
	for i := range 2 {
		report, err := f.engine.Run(t.Context(), []Target{target})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if err := report.Err(); err != nil {
			t.Fatalf("run %d leg failed: %v", i, err)
		}
		if report.Legs[0].SnapshotID == "" {
			t.Errorf("run %d: no snapshot id parsed", i)
		}
		if report.Legs[0].State != store.BackupSucceeded {
			t.Errorf("run %d: state = %s", i, report.Legs[0].State)
		}
	}

	// keyring.age and RECOVERY.md live at the target root, beside the
	// repository rather than inside it.
	for _, name := range []string{KeyringName, RecoveryName, "config"} {
		if _, err := os.Stat(filepath.Join(repo, name)); err != nil {
			t.Errorf("target root: %v", err)
		}
	}
	recovery, err := os.ReadFile(filepath.Join(repo, RecoveryName))
	if err != nil {
		t.Fatalf("read recovery notes: %v", err)
	}
	if got := targetNameFromRecovery(recovery); got != "nas" {
		t.Errorf("target name from RECOVERY.md = %q, want nas", got)
	}
	if bytes.Contains(recovery, []byte(canaryValue)) {
		t.Error("RECOVERY.md leaked the canary")
	}

	runs, err := f.store.ListBackupRuns(t.Context(), target.ID, 0)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("backup_run rows = %d, want 2", len(runs))
	}
	for _, run := range runs {
		if run.State != store.BackupSucceeded || run.Error != "" {
			t.Errorf("run %d: state %s error %q", run.ID, run.State, run.Error)
		}
	}

	dest := filepath.Join(t.TempDir(), "restored")
	manifest, err := RestoreLatest(t.Context(), Source{
		Kind:         KindResticLocal,
		URI:          repo,
		IdentityFile: f.identity,
		Env:          map[string]string{"RESTIC_CACHE_DIR": f.cache},
	}, dest)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(manifest.Files) != 2 {
		t.Errorf("restored manifest lists %d files, want 2", len(manifest.Files))
	}
	assertRestoredChain(t, f, dest, "nas")
}

// assertRestoredChain walks the documented recovery path against a restored
// data dir: the age CLI unwraps the keyring, HKDF derives what the envelope
// derives, and the canary opens with off-the-shelf AES-256-GCM.
func assertRestoredChain(t *testing.T, f *fixture, dest, targetName string) {
	t.Helper()

	for _, name := range []string{RestoredDBName, KeyringName, ManifestName} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Fatalf("restored data dir: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "host.key")); err == nil {
		t.Fatal("restore wrote a host identity; the new host must mint its own")
	}

	// The age CLI, not the Go library: the recovery card promises this works.
	dek, err := exec.Command("age", "-d", "-i", f.identity, filepath.Join(dest, KeyringName)).Output()
	if err != nil {
		t.Fatalf("age -d on the restored keyring: %v", err)
	}
	if len(dek) != dekLen {
		t.Fatalf("age returned %d bytes, want %d", len(dek), dekLen)
	}

	password, err := ResticPassword(dek, targetName)
	if err != nil {
		t.Fatalf("derive restic password: %v", err)
	}
	if want := base64.StdEncoding.EncodeToString(f.env.Derive("restic-repo", targetName)); password != want {
		t.Error("password derived from the restored keyring differs from the envelope's")
	}

	st, err := store.Open(filepath.Join(dest, RestoredDBName))
	if err != nil {
		t.Fatalf("open restored store: %v", err)
	}
	defer func() { _ = st.Close() }()

	row, err := st.GetSecret(t.Context(), "canary")
	if err != nil {
		t.Fatalf("read canary row: %v", err)
	}
	plaintext, err := OpenSealed(dek, row.Name, row.Kind, row.Nonce, row.Ciphertext)
	if err != nil {
		t.Fatalf("open canary: %v", err)
	}
	if string(plaintext) != canaryValue {
		t.Errorf("canary = %q, want %q", plaintext, canaryValue)
	}

	n, err := VerifyCanary(t.Context(), dest, f.identity, "canary")
	if err != nil {
		t.Fatalf("VerifyCanary: %v", err)
	}
	if n != len(canaryValue) {
		t.Errorf("VerifyCanary reported %d bytes, want %d", n, len(canaryValue))
	}
}

func TestLegsFailIndependently(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	good := f.target(t, "good", KindResticLocal, filepath.Join(t.TempDir(), "repo"))
	bad := f.target(t, "bad", KindResticLocal, filepath.Join(blocker, "repo"))

	report, err := f.engine.Run(t.Context(), []Target{bad, good})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := report.Err(); err == nil {
		t.Fatal("report reported success with an unreachable target")
	}
	if len(report.Legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(report.Legs))
	}
	if report.Legs[0].Err == nil || report.Legs[0].State != store.BackupFailed {
		t.Errorf("unreachable leg: state %s err %v", report.Legs[0].State, report.Legs[0].Err)
	}
	if report.Legs[1].Err != nil || report.Legs[1].State != store.BackupSucceeded {
		t.Errorf("reachable leg: state %s err %v", report.Legs[1].State, report.Legs[1].Err)
	}

	badRuns, err := f.store.ListBackupRuns(t.Context(), bad.ID, 1)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(badRuns) != 1 || badRuns[0].State != store.BackupFailed || badRuns[0].Error == "" {
		t.Fatalf("failed target row = %+v", badRuns)
	}
	goodRuns, err := f.store.ListBackupRuns(t.Context(), good.ID, 1)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(goodRuns) != 1 || goodRuns[0].State != store.BackupSucceeded {
		t.Fatalf("reachable target row = %+v", goodRuns)
	}
}

// cancellingDriver is the subprocess a forced shutdown kills: the context is
// cancelled while the leg is running, which is exactly when the engine still
// has its backup_run row to close.
type cancellingDriver struct{ cancel context.CancelFunc }

func (d cancellingDriver) Run(ctx context.Context, _ executor.Spec, _ executor.EventSink) (executor.Result, error) {
	d.cancel()
	<-ctx.Done()
	return executor.Result{Signaled: true}, ctx.Err()
}

// TestCancelledLegFinishesItsRow covers the forced-cancel path: a daemon whose
// drain grace runs out cancels the leg, and the row the leg opened must still
// reach a terminal state. A backup_run left running is a run nothing is
// running, and nothing else in the system will ever move it.
func TestCancelledLegFinishesItsRow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.engine.opts.Driver = cancellingDriver{cancel: cancel}
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "nas"))

	report, err := f.engine.Run(ctx, []Target{target})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Legs[0].State != store.BackupCancelled {
		t.Errorf("leg state = %s, want %s", report.Legs[0].State, store.BackupCancelled)
	}

	runs, err := f.store.ListBackupRuns(context.Background(), target.ID, 1)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("backup_run rows = %d, want 1", len(runs))
	}
	if runs[0].State == store.BackupRunning {
		t.Errorf("backup_run %d is still running after the leg was cancelled", runs[0].ID)
	}
	if runs[0].State != store.BackupCancelled || runs[0].FinishedAt == nil {
		t.Errorf("backup_run %d = state %s finished_at %v, want %s and a stamp",
			runs[0].ID, runs[0].State, runs[0].FinishedAt, store.BackupCancelled)
	}
}

func TestRestoreRefusesInitializedDest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	repo := filepath.Join(t.TempDir(), "nas")
	target := f.target(t, "nas", KindResticLocal, repo)
	report, err := f.engine.Run(t.Context(), []Target{target})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := report.Err(); err != nil {
		t.Fatalf("leg failed: %v", err)
	}

	// The live data dir already holds host.key and keyring.age.
	_, err = RestoreLatest(t.Context(), Source{
		Kind:         KindResticLocal,
		URI:          repo,
		IdentityFile: f.identity,
		Env:          map[string]string{"RESTIC_CACHE_DIR": f.cache},
	}, f.dataDir)
	if err == nil {
		t.Fatal("restore overwrote an initialized data dir")
	}
	if !strings.Contains(err.Error(), "already holds a keyring") {
		t.Errorf("error = %v, want the destination guard", err)
	}
}

func TestRestoreRejectsWrongIdentity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	repo := filepath.Join(t.TempDir(), "nas")
	target := f.target(t, "nas", KindResticLocal, repo)
	if _, err := f.engine.Run(t.Context(), []Target{target}); err != nil {
		t.Fatalf("run: %v", err)
	}

	otherDir := filepath.Join(t.TempDir(), "other")
	_, otherKit, err := secrets.Init(otherDir, nil)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	wrong := filepath.Join(t.TempDir(), "wrong.key")
	if err := os.WriteFile(wrong, []byte(otherKit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}

	_, err = RestoreLatest(t.Context(), Source{
		Kind:         KindResticLocal,
		URI:          repo,
		IdentityFile: wrong,
		Env:          map[string]string{"RESTIC_CACHE_DIR": f.cache},
	}, filepath.Join(t.TempDir(), "restored"))
	if err == nil {
		t.Fatal("restore accepted an identity that is not on the keyring")
	}
}

func TestResticPasswordMatchesEnvelope(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data")
	env, kit, err := secrets.Init(dir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	defer func() { _ = env.Close() }()

	identity := filepath.Join(t.TempDir(), "recovery.key")
	if err := os.WriteFile(identity, []byte(kit.AgeSecretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	ids, err := ReadIdentities(identity)
	if err != nil {
		t.Fatalf("ReadIdentities: %v", err)
	}
	keyring, err := env.KeyringAge()
	if err != nil {
		t.Fatalf("KeyringAge: %v", err)
	}
	dek, err := UnwrapDEK(keyring, ids)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}

	for _, name := range []string{"nas", "offsite-s3", "target with spaces"} {
		got, err := ResticPassword(dek, name)
		if err != nil {
			t.Fatalf("ResticPassword(%q): %v", name, err)
		}
		want := base64.StdEncoding.EncodeToString(env.Derive("restic-repo", name))
		if got != want {
			t.Errorf("ResticPassword(%q) = %s, envelope derives %s", name, got, want)
		}
	}

	sealed, err := env.Seal("db", "password", []byte("hunter2"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	pt, err := OpenSealed(dek, sealed.Name, sealed.Kind, sealed.Nonce, sealed.Ciphertext)
	if err != nil {
		t.Fatalf("OpenSealed: %v", err)
	}
	if string(pt) != "hunter2" {
		t.Errorf("OpenSealed = %q", pt)
	}
	if _, err := OpenSealed(dek, "other", sealed.Kind, sealed.Nonce, sealed.Ciphertext); err == nil {
		t.Error("OpenSealed ignored the name bound as additional data")
	}
}

func TestParseRetention(t *testing.T) {
	t.Parallel()
	cases := []struct {
		spec string
		want []string
		err  bool
	}{
		{spec: "", want: defaultRetention},
		{spec: "none", want: nil},
		{spec: "daily=7,weekly=4", want: []string{"--keep-daily", "7", "--keep-weekly", "4"}},
		{spec: " monthly = 6 ", want: []string{"--keep-monthly", "6"}},
		{spec: "daily", err: true},
		{spec: "fortnightly=2", err: true},
		{spec: "daily=-1", err: true},
		{spec: "daily=x", err: true},
	}
	for _, c := range cases {
		got, err := parseRetention(c.spec)
		if c.err {
			if err == nil {
				t.Errorf("parseRetention(%q) accepted an invalid policy", c.spec)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRetention(%q): %v", c.spec, err)
			continue
		}
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("parseRetention(%q) = %v, want %v", c.spec, got, c.want)
		}
	}
}

func TestParseSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bare", "objects"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cases := []struct {
		spec string
		kind string
		uri  string
	}{
		{"restic_local:/srv/backups", KindResticLocal, "/srv/backups"},
		{"git:/srv/repo", KindGit, "/srv/repo"},
		{"s3:http://127.0.0.1:9000/bucket", KindResticS3, "s3:http://127.0.0.1:9000/bucket"},
		{"git@example.invalid:ops/backup.git", KindGit, "git@example.invalid:ops/backup.git"},
		{"/srv/backups", KindResticLocal, "/srv/backups"},
		{filepath.Join(dir, "bare"), KindGit, filepath.Join(dir, "bare")},
	}
	for _, c := range cases {
		got, err := ParseSource(c.spec)
		if err != nil {
			t.Errorf("ParseSource(%q): %v", c.spec, err)
			continue
		}
		if got.Kind != c.kind || got.URI != c.uri {
			t.Errorf("ParseSource(%q) = %s %s, want %s %s", c.spec, got.Kind, got.URI, c.kind, c.uri)
		}
	}
	if _, err := ParseSource("  "); err == nil {
		t.Error("ParseSource accepted an empty source")
	}
}

func TestUnknownKindFailsItsLegOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	bogus := f.target(t, "bogus", "ftp", "ftp://example.invalid")

	report, err := f.engine.Run(t.Context(), []Target{bogus})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.Legs[0].Err == nil {
		t.Fatal("unknown kind succeeded")
	}
	if !strings.Contains(report.Legs[0].Err.Error(), "unknown target kind") {
		t.Errorf("error = %v", report.Legs[0].Err)
	}
}
