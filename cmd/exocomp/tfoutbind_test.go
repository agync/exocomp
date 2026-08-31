package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// tfOutSentinel stands in for the value a `sensitive = true` tofu output
// carries. Nothing but its length may reach an event, an argv, or the ledger.
const tfOutSentinel = "exocomp-tfout-sentinel-7f21ab-do-not-log"

// sealWorkspaceOutput runs the producer half against a data dir: seal one
// sensitive output for a workspace and bind it, exactly as an apply's capture
// does, minus tofu.
func sealWorkspaceOutput(t *testing.T, dir, workspace, output, value string) {
	t.Helper()
	st, env := openTfOutStore(t, dir)
	unbound, err := executor.EnvelopeOutputSealer{
		Env: env, Store: st, Workspace: workspace,
	}.SealOutput(context.Background(), output, []byte(value))
	if err != nil {
		t.Fatalf("seal output %q: %v", output, err)
	}
	if unbound != "" {
		t.Fatalf("output %q was not bound: %s", output, unbound)
	}
}

// openTfOutStore opens the data dir's store and envelope for one test.
func openTfOutStore(t *testing.T, dir string) (*store.Store, *secrets.Envelope) {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })
	return st, env
}

// TestTofuOutputBindingReachesDependentPlay is the item-D consumer gate: a play
// that targets a workspace's standing `tf:<workspace>` inventory resolves that
// workspace's sealed outputs as named env with no flag and nothing copied,
// while a run targeting anything else resolves none of them.
func TestTofuOutputBindingReachesDependentPlay(t *testing.T) {
	dir := initForSource(t)
	ctx := context.Background()
	sealWorkspaceOutput(t, dir, "edge", "db_password", tfOutSentinel)

	st, env := openTfOutStore(t, dir)
	inventoryPath := filepath.Join(t.TempDir(), "edge.json")
	if err := os.WriteFile(inventoryPath, []byte(`{"all":{"hosts":{}}}`), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	for _, ws := range []string{"edge", "other"} {
		if _, err := st.MaterializeInventory(ctx, handoffInventoryName(ws), inventoryPath); err != nil {
			t.Fatalf("materialize inventory: %v", err)
		}
	}

	rs, plain, err := resolveRunSecrets(ctx, st, env, "", handoffInventoryName("edge"))
	if err != nil {
		t.Fatalf("resolveRunSecrets: %v", err)
	}
	if rs["DB_PASSWORD"] != tfOutSentinel {
		t.Fatalf("DB_PASSWORD = %q, want the sealed output value", rs["DB_PASSWORD"])
	}
	if len(plain) != 1 {
		t.Errorf("plaintexts = %d, want 1 (the redactor scrubs it)", len(plain))
	}

	// A run in another workspace's chain, or in no chain at all, is untouched:
	// the binding is scoped by the inventory the run names.
	for _, inv := range []string{handoffInventoryName("other"), ""} {
		other, _, err := resolveRunSecrets(ctx, st, env, "", inv)
		if err != nil {
			t.Fatalf("resolveRunSecrets(%q): %v", inv, err)
		}
		if _, ok := other["DB_PASSWORD"]; ok {
			t.Errorf("inventory %q inherited another workspace's sealed output", inv)
		}
	}
}

// TestTofuOutputBindingRunsAPlayValuesFree is the values-free half of the gate:
// the play really receives the sealed value as $DB_PASSWORD — proven by its
// length, never its text — while the argv the job row records and every durable
// event stay free of the value itself.
func TestTofuOutputBindingRunsAPlayValuesFree(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	if _, err := exec.LookPath(executor.AnsibleBinary); err != nil {
		t.Skipf("%s not installed: %v", executor.AnsibleBinary, err)
	}
	dir := initForSource(t)
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	sealWorkspaceOutput(t, dir, "edge", "db_password", tfOutSentinel)

	st, env := openTfOutStore(t, dir)
	inventoryPath := filepath.Join(t.TempDir(), "edge.json")
	if err := os.WriteFile(inventoryPath, []byte(`{"all":{"hosts":{}}}`), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	if _, err := st.MaterializeInventory(ctx, handoffInventoryName("edge"), inventoryPath); err != nil {
		t.Fatalf("materialize inventory: %v", err)
	}

	rs, plain, err := resolveRunSecrets(ctx, st, env, "", handoffInventoryName("edge"))
	if err != nil {
		t.Fatalf("resolveRunSecrets: %v", err)
	}

	playbook := filepath.Join(t.TempDir(), "converge.yml")
	body := `- name: exocomp bound-output gate
  hosts: localhost
  gather_facts: false
  tasks:
    - name: report the bound secret by length, never by value
      ansible.builtin.debug:
        msg: "dbpwlen={{ lookup('env', 'DB_PASSWORD') | length }}"
`
	if err := os.WriteFile(playbook, []byte(body), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	drv := executor.NewAnsible(executor.AnsibleOptions{
		PrivateDirRoot: t.TempDir(),
		RunSecrets:     rs,
	})
	q := queue.New(st, drv, queue.Options{Workers: 1, PollInterval: 20 * time.Millisecond},
		queue.WithRedactor(secrets.NewRedactor(plain)))
	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook:   "playbook",
		Kind:      executor.KindAnsible,
		Trigger:   "test",
		Argv:      executor.AnsibleArgv(playbook, ""),
		Inventory: handoffInventoryName("edge"),
	})
	if err != nil {
		t.Fatalf("enqueue play: %v", err)
	}
	if err := q.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	final, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	events, err := st.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var log strings.Builder
	for _, e := range events {
		log.WriteString(e.Msg)
		log.WriteString("\n")
		log.WriteString(e.RawJSON)
		log.WriteString("\n")
	}
	if final.State != store.StateSucceeded {
		t.Fatalf("play ended %s:\n%s", final.State, log.String())
	}
	// The play saw the value: it reported its length, which only an injected
	// env var can produce.
	want := "dbpwlen=" + strconv.Itoa(len(tfOutSentinel))
	if !strings.Contains(log.String(), want) {
		t.Errorf("play did not report %q:\n%s", want, log.String())
	}
	// And nothing anywhere carries the value itself.
	if strings.Contains(log.String(), tfOutSentinel) {
		t.Errorf("an event carried the sealed value:\n%s", log.String())
	}
	if strings.Contains(strings.Join(final.Args.Argv, " "), tfOutSentinel) {
		t.Errorf("argv carried the sealed value: %q", final.Args.Argv)
	}
	if final.Args.RunSecretBinding != "" {
		t.Errorf("job row named a binding it was never given: %q", final.Args.RunSecretBinding)
	}
}
