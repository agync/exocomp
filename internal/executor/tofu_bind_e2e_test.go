package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// rotatedCanary is what the second apply's sensitive output holds, so a
// re-apply can be told apart from a first one by value, not just by row count.
const rotatedCanary = "exocomp-sensitive-output-rotated-4b71c2"

// TestTofuApplyBindsSealedOutput is the item-D producer gate: an apply that
// emits a sensitive output seals it AND binds it to an env name under the
// workspace's `tf:<workspace>` binding, a re-apply updates that binding in
// place rather than duplicating it, and an output whose name cannot be a POSIX
// identifier is sealed but refused a binding — with a reason, not a mangled
// name.
func TestTofuApplyBindsSealedOutput(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()

	base := readConfig(t, f.tofuDir)
	writeBindOutputs(t, f.tofuDir, base, sensitiveCanary)

	capture := applyWithSealer(t, f, filepath.Join(t.TempDir(), "edge-1.json"))

	// Both sensitive outputs are sealed; only the one whose name can be an env
	// identifier is bound.
	for _, name := range []string{"db_password", "db-password"} {
		if !contains(capture.Sealed, name) {
			t.Errorf("capture did not seal %q: %+v", name, capture)
		}
	}
	if len(capture.Unbound) != 1 || !strings.Contains(capture.Unbound[0], "db-password") {
		t.Fatalf("capture did not report the unbindable output: %+v", capture)
	}
	if !strings.Contains(capture.Unbound[0], "POSIX identifier") {
		t.Errorf("unbound reason does not say why: %q", capture.Unbound[0])
	}

	binding := executor.TofuOutputBinding(f.workspace.Name)
	pairs, err := f.store.RunSecretsByBinding(ctx, binding)
	if err != nil {
		t.Fatalf("run secrets by binding: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("binding %q holds %d pair(s), want 1: %+v", binding, len(pairs), pairs)
	}
	if pairs[0].EnvName != "DB_PASSWORD" {
		t.Errorf("env name = %q, want DB_PASSWORD", pairs[0].EnvName)
	}
	if want := executor.TofuOutputSecretName(f.workspace.Name, "db_password"); pairs[0].SecretName != want {
		t.Errorf("secret name = %q, want %q", pairs[0].SecretName, want)
	}
	// The refused output was not rewritten into some other env name.
	if _, err := f.store.GetSecret(ctx, executor.TofuOutputSecretName(f.workspace.Name, "db-password")); err != nil {
		t.Errorf("the unbindable output was not sealed either: %v", err)
	}

	// A re-apply with a rotated value updates the sealed row and leaves the
	// binding alone: one pair, still pointing at the same deterministic name.
	writeBindOutputs(t, f.tofuDir, base, rotatedCanary)
	applyWithSealer(t, f, filepath.Join(t.TempDir(), "edge-2.json"))

	pairs, err = f.store.RunSecretsByBinding(ctx, binding)
	if err != nil {
		t.Fatalf("run secrets by binding: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("re-apply duplicated the binding: %d pair(s): %+v", len(pairs), pairs)
	}
	if got := openSealedOutput(t, f, pairs[0].SecretName); got != rotatedCanary {
		t.Errorf("sealed value = %q, want the rotated canary", got)
	}
}

// applyWithSealer runs one apply with the binding sealer wired and returns the
// capture summary it emitted.
func applyWithSealer(t *testing.T, f *fixture, outPath string) executor.InventoryCapture {
	t.Helper()
	ctx := context.Background()
	drv := executor.NewTofu(executor.TofuOptions{
		Workspace:        f.workspace.Name,
		BackendURL:       api.StateURL(f.server.Addr(), f.workspace.Name),
		BackendToken:     f.token,
		StatePassphrase:  f.passphrase,
		PrivateDirRoot:   t.TempDir(),
		PluginCacheDir:   tofuPluginCache(t),
		OfflineProviders: true,
		InventoryOut:     outPath,
		OutputSealer: executor.EnvelopeOutputSealer{
			Env: f.env, Store: f.store, Workspace: f.workspace.Name,
		},
	})
	// The redactor knows only the envelope material, deliberately NOT the
	// sensitive values: their absence from the log must be the capture routing
	// them away, not the redactor scrubbing them after the fact.
	q := queue.New(f.store, drv, queue.Options{Workers: 1, PollInterval: 20 * time.Millisecond},
		queue.WithRedactor(secrets.NewRedactor([][]byte{[]byte(f.token), []byte(f.passphrase)})))

	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook:        "workspace:" + f.workspace.Name,
		Kind:           executor.KindTofu,
		Trigger:        "test",
		Argv:           executor.TofuArgv("apply", "-auto-approve"),
		Dir:            f.tofuDir,
		ConcurrencyKey: "tofu:" + f.workspace.Name,
	})
	if err != nil {
		t.Fatalf("enqueue apply: %v", err)
	}
	f.server.BindJob(f.workspace.Name, job.ID)
	defer f.server.ReleaseJob(f.workspace.Name)
	if err := q.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	final, err := f.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if final.State != store.StateSucceeded {
		t.Fatalf("apply ended %s:\n%s", final.State, f.log(job.ID))
	}
	for _, canary := range []string{sensitiveCanary, rotatedCanary} {
		if log := f.log(job.ID); strings.Contains(log, canary) {
			t.Fatalf("an event carried a sealed output value:\n%s", log)
		}
	}
	return findCapture(t, f, job.ID)
}

func openSealedOutput(t *testing.T, f *fixture, name string) string {
	t.Helper()
	row, err := f.store.GetSecret(context.Background(), name)
	if err != nil {
		t.Fatalf("sealed output %q not stored: %v", name, err)
	}
	pt, err := f.env.OpenSecret(secrets.SealedSecret{
		Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open sealed output: %v", err)
	}
	return string(pt)
}

func readConfig(t *testing.T, tofuDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(tofuDir, "main.tf"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return string(raw)
}

// writeBindOutputs rewrites the fixture's configuration with two sensitive
// outputs — one whose name folds to a valid env identifier and one whose does
// not — plus the inventory output, so one apply exercises both bind paths.
func writeBindOutputs(t *testing.T, tofuDir, base, canary string) {
	t.Helper()
	body := base + `
output "db_password" {
  value     = "` + canary + `"
  sensitive = true
}
output "db-password" {
  value     = "` + canary + `-dashed"
  sensitive = true
}
output "ansible_inventory" {
  value = {
    all = { hosts = { localhost = { ansible_connection = "local" } } }
  }
}
`
	if err := os.WriteFile(filepath.Join(tofuDir, "main.tf"), []byte(body), 0o600); err != nil {
		t.Fatalf("write outputs: %v", err)
	}
}
