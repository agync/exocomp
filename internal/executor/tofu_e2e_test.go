package executor_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// canary is the resource value the state must never hold in the clear.
const canary = "exocomp-state-canary-2f8a1c"

// tofuPluginCache returns a machine-persistent provider cache, warmed exactly
// once. The fixtures resolve providers from it offline (OfflineProviders), so
// after the first warm the live-provider e2e no longer touches the registry —
// its previous flake was `tofu init` doing online version discovery while the
// network dropped mid-run, which no reachability probe can prevent. Warming is
// the sole online step: it needs the registry only when the cache is cold, and
// a cold cache with an unreachable registry SKIPS (the offline path has its own
// no-network proof in tofu_offline_test.go), never fails.
func tofuPluginCache(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), "exocomp-tofu-plugin-cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("plugin cache: %v", err)
	}
	tofuWarmOnce.Do(func() { tofuWarmSkip = warmTofuCache(dir) })
	if tofuWarmSkip != "" {
		t.Skip(tofuWarmSkip)
	}
	return dir
}

var (
	tofuWarmOnce sync.Once
	tofuWarmSkip string // non-empty is a skip reason; empty means warm and offline-ready
)

// warmTofuCache ensures the hashicorp/local provider is present in cacheDir so
// offline resolution can satisfy it. It returns "" on success, or a skip reason
// when tofu is absent or the cache is cold and the registry is unreachable. A
// warm cache short-circuits with no network at all.
func warmTofuCache(cacheDir string) string {
	if _, err := exec.LookPath(executor.TofuBinary); err != nil {
		return fmt.Sprintf("%s not installed: %v", executor.TofuBinary, err)
	}
	glob := filepath.Join(cacheDir, "registry.opentofu.org", "hashicorp", "local", "*", "*", "terraform-provider-local*")
	if hits, _ := filepath.Glob(glob); len(hits) > 0 {
		return ""
	}
	// Cold cache: one network fetch populates it. Probe first so an unreachable
	// registry skips cleanly instead of a slow init failure.
	c := &http.Client{Timeout: 6 * time.Second}
	resp, err := c.Get("https://registry.opentofu.org/.well-known/terraform.json")
	if err != nil {
		return fmt.Sprintf("provider registry unreachable and plugin cache cold, skipping live-provider e2e: %v", err)
	}
	_ = resp.Body.Close()

	work, err := os.MkdirTemp("", "exocomp-tofu-warm")
	if err != nil {
		return fmt.Sprintf("warm plugin cache: workdir: %v", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	config := "terraform {\n  required_providers {\n    local = {\n      source = \"hashicorp/local\"\n    }\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(work, "main.tf"), []byte(config), 0o600); err != nil {
		return fmt.Sprintf("warm plugin cache: config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executor.TofuBinary, "init", "-backend=false", "-input=false")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "TF_PLUGIN_CACHE_DIR="+cacheDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("warm plugin cache: tofu init: %v\n%s", err, out)
	}
	return ""
}

func requireTofu(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(executor.TofuBinary); err != nil {
		t.Skipf("%s not installed: %v", executor.TofuBinary, err)
	}
}

// fixture is a whole tofu half in miniature: a workspace directory managing a
// local file, the store behind it, the state backend serving it, and the queue
// that runs it.
type fixture struct {
	t          *testing.T
	store      *store.Store
	env        *secrets.Envelope
	server     *api.Server
	workspace  store.Workspace
	tofuDir    string
	target     string
	queue      *queue.Queue
	driver     *executor.Tofu
	token      string
	passphrase string
}

func newFixture(t *testing.T, workers int) *fixture {
	t.Helper()
	requireTofu(t)

	dataDir := t.TempDir()
	tofuDir := t.TempDir()
	config := "resource \"local_file\" \"greeting\" {\n" +
		"  filename = \"${path.module}/greeting.txt\"\n" +
		"  content  = \"" + canary + "\"\n}\n"
	if err := os.WriteFile(filepath.Join(tofuDir, "main.tf"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	st, err := store.Open(filepath.Join(dataDir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	env, _, err := secrets.Init(dataDir, nil)
	if err != nil {
		t.Fatalf("init envelope: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })

	ws, err := st.CreateWorkspace(context.Background(), store.Workspace{
		Name:    "edge",
		TofuDir: tofuDir,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	srv, err := api.New(api.Options{Store: st, Tokens: env, Port: 0})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start backend: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown backend: %v", err)
		}
	})

	token := api.WorkspaceToken(env, ws.Name)
	stateKey := env.Derive(executor.TofuStatePurpose, ws.Name)
	passphrase := base64.StdEncoding.EncodeToString(stateKey)

	driver := executor.NewTofu(executor.TofuOptions{
		Workspace:       ws.Name,
		BackendURL:      api.StateURL(srv.Addr(), ws.Name),
		BackendToken:    token,
		StatePassphrase: passphrase,
		PrivateDirRoot:  t.TempDir(),
		PluginCacheDir:  tofuPluginCache(t),
		// Resolve providers from the warmed cache alone: no online version
		// discovery during init, so the run is deterministic and network-free.
		OfflineProviders: true,
	})
	q := queue.New(st, driver, queue.Options{
		Workers:      workers,
		PollInterval: 20 * time.Millisecond,
	}, queue.WithRedactor(secrets.NewRedactor([][]byte{
		[]byte(token), stateKey, []byte(passphrase),
	})))

	return &fixture{
		t:          t,
		store:      st,
		env:        env,
		server:     srv,
		workspace:  ws,
		tofuDir:    tofuDir,
		target:     filepath.Join(tofuDir, "greeting.txt"),
		queue:      q,
		driver:     driver,
		token:      token,
		passphrase: passphrase,
	}
}

// run enqueues one tofu operation, drains the queue, and returns the job with
// everything it logged.
func (f *fixture) run(argv []string, concurrencyKey string) (store.Job, string) {
	f.t.Helper()
	ctx := context.Background()

	job, err := f.queue.Enqueue(ctx, queue.Spec{
		Runbook:        "workspace:" + f.workspace.Name,
		Kind:           executor.KindTofu,
		Trigger:        "test",
		Argv:           argv,
		Dir:            f.workspace.TofuDir,
		ConcurrencyKey: concurrencyKey,
	})
	if err != nil {
		f.t.Fatalf("enqueue %v: %v", argv, err)
	}
	f.server.BindJob(f.workspace.Name, job.ID)
	defer f.server.ReleaseJob(f.workspace.Name)

	if err := f.queue.Drain(ctx); err != nil {
		f.t.Fatalf("drain: %v", err)
	}

	final, err := f.store.GetJob(ctx, job.ID)
	if err != nil {
		f.t.Fatalf("get job: %v", err)
	}
	return final, f.log(job.ID)
}

func (f *fixture) log(jobID int64) string {
	f.t.Helper()
	events, err := f.store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		f.t.Fatalf("list events: %v", err)
	}
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e.Level + " " + e.Msg + "\n")
	}
	return b.String()
}

// planFoundChanges reads the drift verdict back out of the job's events.
func (f *fixture) planFoundChanges(jobID int64) bool {
	f.t.Helper()
	events, err := f.store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		f.t.Fatalf("list events: %v", err)
	}
	for _, e := range events {
		if changes, ok := executor.TofuPlanChanges(e.RawJSON); ok {
			return changes
		}
	}
	f.t.Fatalf("job %d recorded no plan outcome:\n%s", jobID, f.log(jobID))
	return false
}

// addResource appends a second managed file to the configuration, so the next
// apply has something real to record.
func (f *fixture) addResource(name, content string) {
	f.t.Helper()
	body := "\nresource \"local_file\" \"" + name + "\" {\n" +
		"  filename = \"${path.module}/" + name + ".txt\"\n" +
		"  content  = \"" + content + "\"\n}\n"

	path := filepath.Join(f.tofuDir, "main.tf")
	existing, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(path, append(existing, body...), 0o600); err != nil {
		f.t.Fatalf("extend config: %v", err)
	}
}

func (f *fixture) requireSucceeded(job store.Job, log string) {
	f.t.Helper()
	if job.State != store.StateSucceeded {
		f.t.Fatalf("job %d ended %s:\n%s", job.ID, job.State, log)
	}
}

// TestTofuAdoptsExistingLocalState proves the adoption runbook end to end: a
// workspace whose tofu dir already holds state from a prior `backend "local"`
// is migrated onto the HTTP backend by one AdoptState run. The unencrypted
// fallback lets init read the old terraform.tfstate, -migrate-state moves it,
// and a subsequent plain plan reads clean state — proving the fallback was not
// needed again. This is the regression test for G1.
func TestTofuAdoptsExistingLocalState(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real tofu")
	}
	if _, err := exec.LookPath(executor.TofuBinary); err != nil {
		t.Skip("tofu not installed")
	}
	f := newFixture(t, 1)
	ctx := context.Background()

	// Establish local state OUTSIDE exocomp: a local backend in the same tofu
	// dir, applied once, leaving terraform.tfstate (serial 1, unencrypted).
	localCfg := "terraform {\n  backend \"local\" {}\n}\n"
	extra := filepath.Join(f.tofuDir, "backend.tf")
	if err := os.WriteFile(extra, []byte(localCfg), 0o600); err != nil {
		t.Fatalf("write local backend: %v", err)
	}
	runLocal := func(argv ...string) {
		cmd := exec.CommandContext(ctx, executor.TofuBinary, argv...)
		cmd.Dir = f.tofuDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("local tofu %v: %v", argv, err)
		}
	}
	runLocal("init", "-input=false")
	runLocal("plan", "-out="+filepath.Join(f.tofuDir, "adopt.tfplan"), "-input=false")
	runLocal("apply", "-auto-approve", filepath.Join(f.tofuDir, "adopt.tfplan"))
	localState := filepath.Join(f.tofuDir, "terraform.tfstate")
	if _, err := os.Stat(localState); err != nil {
		t.Fatalf("local state not created: %v", err)
	}
	if err := os.Remove(extra); err != nil {
		t.Fatalf("remove local backend: %v", err)
	}

	// A normal (non-adopt) run must refuse: tofu needs a migration flag to change
	// backends, and the driver never passes one outside adoption.
	refused, refusedLog := f.run(executor.TofuArgv("plan"), "tofu:edge")
	if refused.State != store.StateFailed {
		t.Fatalf("non-adopt plan over existing local state ended %s, want failed:\n%s",
			refused.State, refusedLog)
	}

	// The adoption run: AdoptState widens the encryption doc and passes
	// -migrate-state; the state lands in the HTTP backend at serial 1.
	f.driver = executor.NewTofu(executor.TofuOptions{
		Workspace:        f.workspace.Name,
		BackendURL:       api.StateURL(f.server.Addr(), f.workspace.Name),
		BackendToken:     f.token,
		StatePassphrase:  f.passphrase,
		PrivateDirRoot:   f.t.TempDir(),
		PluginCacheDir:   tofuPluginCache(f.t),
		OfflineProviders: true,
		AdoptState:       true,
	})
	f.queue = queue.New(f.store, f.driver, queue.Options{
		Workers:      1,
		PollInterval: 20 * time.Millisecond,
	}, queue.WithRedactor(secrets.NewRedactor([][]byte{
		[]byte(f.token), []byte(f.passphrase),
	})))

	adoptJob, adoptLog := f.run(executor.TofuArgv("init"), "tofu:edge")
	f.requireSucceeded(adoptJob, adoptLog)
	version, err := f.store.LatestTFState(ctx, f.workspace.ID)
	if err != nil {
		t.Fatalf("latest tfstate after adopt: %v", err)
	}
	if version.Serial != 1 {
		t.Errorf("serial after adopt = %d, want 1", version.Serial)
	}
	// Tofu's documented migration safety: the old local file is drained to an
	// empty placeholder and the pre-migration state is preserved at .backup.
	// What matters is the state is no longer authoritative locally.
	if info, err := os.Stat(localState); err != nil {
		t.Fatalf("local terraform.tfstate missing after adopt: %v", err)
	} else if info.Size() != 0 {
		t.Errorf("local terraform.tfstate not drained after adopt: %d bytes, want 0", info.Size())
	}
	if _, err := os.Stat(localState + ".backup"); err != nil {
		t.Errorf("migration backup missing at .backup: %v", err)
	}

	// A subsequent plain run reads the adopted state cleanly: no drift, no
	// "unencrypted payload" error, no -migrate-state.
	f.driver = executor.NewTofu(executor.TofuOptions{
		Workspace:        f.workspace.Name,
		BackendURL:       api.StateURL(f.server.Addr(), f.workspace.Name),
		BackendToken:     f.token,
		StatePassphrase:  f.passphrase,
		PrivateDirRoot:   f.t.TempDir(),
		PluginCacheDir:   tofuPluginCache(f.t),
		OfflineProviders: true,
	})
	f.queue = queue.New(f.store, f.driver, queue.Options{
		Workers:      1,
		PollInterval: 20 * time.Millisecond,
	}, queue.WithRedactor(secrets.NewRedactor([][]byte{
		[]byte(f.token), []byte(f.passphrase),
	})))

	planJob, planLog := f.run(executor.TofuArgv("plan"), "tofu:edge")
	f.requireSucceeded(planJob, planLog)
	if f.planFoundChanges(planJob.ID) {
		t.Errorf("plan after adopt found drift against adopted state:\n%s", planLog)
	}
	if strings.Contains(planLog, "-migrate-state") {
		t.Errorf("plain plan after adopt still passed -migrate-state:\n%s", planLog)
	}
	if strings.Contains(planLog, "unencrypted payload") {
		t.Errorf("plain plan after adopt hit an unencrypted payload error:\n%s", planLog)
	}
}

func TestTofuWorkspaceEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real tofu")
	}
	f := newFixture(t, 1)
	ctx := context.Background()

	// A plan reports what would change and applies none of it.
	planJob, planLog := f.run(executor.TofuArgv("plan"), "tofu:edge")
	f.requireSucceeded(planJob, planLog)
	if !f.planFoundChanges(planJob.ID) {
		t.Errorf("plan against empty state found no changes:\n%s", planLog)
	}
	if _, err := os.Stat(f.target); !os.IsNotExist(err) {
		t.Errorf("plan created %s: %v", f.target, err)
	}
	if _, err := f.store.LatestTFState(ctx, f.workspace.ID); err == nil {
		t.Error("plan pushed a state version")
	}

	// The apply is what writes both the resource and the state.
	applyJob, applyLog := f.run(executor.TofuArgv("apply", "-auto-approve"), "tofu:edge")
	f.requireSucceeded(applyJob, applyLog)
	written, err := os.ReadFile(f.target)
	if err != nil {
		t.Fatalf("read managed file: %v", err)
	}
	if string(written) != canary {
		t.Errorf("managed file = %q, want %q", written, canary)
	}

	version, err := f.store.LatestTFState(ctx, f.workspace.ID)
	if err != nil {
		t.Fatalf("latest tfstate: %v", err)
	}
	if version.Serial != 1 {
		t.Errorf("serial = %d, want 1", version.Serial)
	}
	if version.Lineage == "" {
		t.Error("stored version has no lineage")
	}
	if version.CreatedByJob == nil || *version.CreatedByJob != applyJob.ID {
		t.Errorf("created_by_job_id = %v, want %d", version.CreatedByJob, applyJob.ID)
	}

	// The point of the whole arrangement: the row is ciphertext.
	if bytes.Contains(version.Ciphertext, []byte(canary)) {
		t.Error("the state row holds the resource value in the clear")
	}
	if !bytes.Contains(version.Ciphertext, []byte("encrypted_data")) {
		t.Errorf("state row is not a tofu encryption envelope: %.120s", version.Ciphertext)
	}
	// The plan output did carry it, so the absence above is encryption and
	// not a value that never existed.
	if !strings.Contains(applyLog, canary) {
		t.Errorf("apply log never mentioned the resource value:\n%s", applyLog)
	}

	// Reading state back through the CLI proves the backend serves what tofu
	// itself expects, not just what this test wrote.
	listJob, listLog := f.run(executor.TofuArgv("state", "list"), "tofu:edge")
	f.requireSucceeded(listJob, listLog)
	if !strings.Contains(listLog, "local_file.greeting") {
		t.Errorf("state list did not name the resource:\n%s", listLog)
	}

	// Drift: the file changes underneath, and the next plan says so.
	if err := os.WriteFile(f.target, []byte("tampered by hand"), 0o600); err != nil {
		t.Fatalf("mutate managed file: %v", err)
	}
	driftJob, driftLog := f.run(executor.TofuArgv("plan"), "tofu:edge")
	f.requireSucceeded(driftJob, driftLog)
	if !f.planFoundChanges(driftJob.ID) {
		t.Errorf("plan missed the drift:\n%s", driftLog)
	}
	if driftJob.ExitCode == nil || *driftJob.ExitCode != 0 {
		t.Errorf("drift job exit code = %v, want 0: drift is not a failed run", driftJob.ExitCode)
	}

	// Applying again converges the file back to what state says it is.
	fixJob, fixLog := f.run(executor.TofuArgv("apply", "-auto-approve"), "tofu:edge")
	f.requireSucceeded(fixJob, fixLog)
	restored, err := os.ReadFile(f.target)
	if err != nil || string(restored) != canary {
		t.Errorf("managed file = %q (%v), want it converged back to %q", restored, err, canary)
	}

	// A real change to the configuration adds a version rather than replacing
	// one: every serial is kept, which is what makes rollback an ordinary row.
	f.addResource("farewell", "goodbye")
	growJob, growLog := f.run(executor.TofuArgv("apply", "-auto-approve"), "tofu:edge")
	f.requireSucceeded(growJob, growLog)

	versions, err := f.store.ListTFStates(ctx, f.workspace.ID, 0)
	if err != nil {
		t.Fatalf("list tfstates: %v", err)
	}
	if len(versions) != 2 || versions[0].Serial != 2 || versions[1].Serial != 1 {
		t.Fatalf("versions = %d rows with serials %v, want 2 then 1", len(versions), serials(versions))
	}

	// Nothing derived from the envelope may reach the durable event log.
	for _, jobID := range []int64{planJob.ID, applyJob.ID, listJob.ID, driftJob.ID, fixJob.ID, growJob.ID} {
		log := f.log(jobID)
		if strings.Contains(log, f.token) || strings.Contains(log, f.passphrase) {
			t.Errorf("job %d logged material derived from the envelope", jobID)
		}
	}

	// The lock is a lease, not a leak: it is gone once the run is.
	if _, err := f.store.GetTFLock(ctx, f.workspace.ID); err == nil {
		t.Error("a lock outlived the run that took it")
	}
}

func TestTofuAppliesSerializeOnTheConcurrencyKey(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real tofu")
	}
	f := newFixture(t, 2)
	ctx := context.Background()

	var ids []int64
	for i := 0; i < 2; i++ {
		job, err := f.queue.Enqueue(ctx, queue.Spec{
			Runbook:        "workspace:edge",
			Kind:           executor.KindTofu,
			Trigger:        "test",
			Argv:           executor.TofuArgv("apply", "-auto-approve"),
			Dir:            f.workspace.TofuDir,
			ConcurrencyKey: "tofu:" + f.workspace.Name,
		})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		ids = append(ids, job.ID)
	}

	if err := f.queue.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	first, err := f.store.GetJob(ctx, ids[0])
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	second, err := f.store.GetJob(ctx, ids[1])
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	f.requireSucceeded(first, f.log(first.ID))
	f.requireSucceeded(second, f.log(second.ID))

	// Two workers were free, so an overlap would mean the key did nothing.
	if first.FinishedAt == nil || second.StartedAt == nil {
		t.Fatalf("jobs have no clock: %+v %+v", first, second)
	}
	if second.StartedAt.Before(*first.FinishedAt) {
		t.Errorf("job %d started at %s, before job %d finished at %s",
			second.ID, second.StartedAt, first.ID, first.FinishedAt)
	}

	// One apply did the work; the other found nothing to do. Either way the
	// state moved exactly once.
	ws, err := f.store.GetWorkspace(ctx, f.workspace.ID)
	if err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if ws.Serial != 1 {
		t.Errorf("serial after two serialized applies = %d, want 1", ws.Serial)
	}
}

func TestTofuRefusesToRunUnderAForeignLock(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real tofu")
	}
	f := newFixture(t, 1)
	ctx := context.Background()

	if _, err := f.store.AcquireTFLock(ctx, store.TFLock{
		WorkspaceID: f.workspace.ID,
		LockID:      "held-by-someone-else",
		Who:         "other@host",
		Operation:   "OperationTypeApply",
	}); err != nil {
		t.Fatalf("acquire lock: %v", err)
	}

	// No concurrency key: the queue is not what must stop this run. The
	// backend lock is, and it is the guard that also holds against a second
	// control plane the queue knows nothing about.
	job, log := f.run(executor.TofuArgv("apply", "-auto-approve"), "")
	if job.State != store.StateFailed {
		t.Fatalf("job ended %s, want failed:\n%s", job.State, log)
	}
	if !strings.Contains(log, "held-by-someone-else") {
		t.Errorf("the run was not told who holds the lock:\n%s", log)
	}
	if _, err := f.store.LatestTFState(ctx, f.workspace.ID); err == nil {
		t.Error("a run that could not lock still pushed state")
	}
}

func serials(versions []store.TFState) []int64 {
	out := make([]int64, len(versions))
	for i, v := range versions {
		out[i] = v.Serial
	}
	return out
}
