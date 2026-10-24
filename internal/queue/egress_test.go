package queue_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// TestEnqueueCarriesEgressPorts proves Spec.EgressPorts reaches the stored job
// row: the seam a `playbook run --egress` declaration depends on to reach the
// sandbox profile when whichever process claims the job runs it. Empty is the
// default, unaffected path most jobs take. Dropping the field from the Enqueue
// JobArgs literal fails this test.
func TestEnqueueCarriesEgressPorts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	spec := shellSpec("/bin/true")
	spec.EgressPorts = []int{2222}
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(got.Args.EgressPorts, []int{2222}) {
		t.Errorf("stored EgressPorts = %v, want [2222]", got.Args.EgressPorts)
	}
}

// TestEnqueueGatedCarriesEgressPorts is EnqueueGated's half of the hop: a
// --require-approval rule's playbook job must keep its egress declaration once
// released, so the gate cannot silently drop the grant. Dropping the field
// from the gate.go JobArgs literal fails this test.
func TestEnqueueGatedCarriesEgressPorts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	spec := shellSpec("/bin/true")
	spec.EgressPorts = []int{2222}
	gated, err := q.EnqueueGated(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue gated: %v", err)
	}

	got, err := st.GetJob(ctx, gated.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(got.Args.EgressPorts, []int{2222}) {
		t.Errorf("stored EgressPorts = %v, want [2222]", got.Args.EgressPorts)
	}
}

// TestExecuteCarriesEgressPortsToDriver proves the ports persisted on the row
// reach the executor's Spec, the field the sandboxed ansible driver reads to
// widen the profile. Dropping the field from the execute mapping fails this
// test.
func TestExecuteCarriesEgressPortsToDriver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	drv := &recordingDriver{}
	q := queue.New(st, drv, queue.Options{
		Workers:      1,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	spec := shellSpec("/bin/true")
	spec.EgressPorts = []int{2222}
	if _, err := q.Enqueue(ctx, spec); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, q)

	if !slices.Equal(drv.last.EgressPorts, []int{2222}) {
		t.Errorf("driver Spec.EgressPorts = %v, want [2222]", drv.last.EgressPorts)
	}
}

// ansibleSpec builds an ansible job naming source, the shape every ansible
// producer hands to admission.
func ansibleSpec(source string) queue.Spec {
	return queue.Spec{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
		Argv:   []string{"ansible-playbook", "site.yml"},
		Source: source,
	}
}

// newEgressSource registers a source and, when ports is non-empty, records its
// default egress set, through the real store.
func newEgressSource(t *testing.T, st *store.Store, name string, ports []int) {
	t.Helper()
	src, err := st.CreateSource(context.Background(),
		store.Source{Name: name, Kind: store.SourceGit, Enabled: true})
	if err != nil {
		t.Fatalf("create source %q: %v", name, err)
	}
	if len(ports) == 0 {
		return
	}
	if _, err := st.SetSourceEgressPorts(context.Background(), src.ID, ports); err != nil {
		t.Fatalf("set source %q egress: %v", name, err)
	}
}

// TestEnqueueUnionsSourceEgressDefault is the security-critical admission hop
// (Decision 0023): an ansible job naming a source has that source's persisted
// default unioned into its declared ports, so the recorded row is the grant the
// run receives. Dropping the union from Enqueue fails this test.
func TestEnqueueUnionsSourceEgressDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)
	newEgressSource(t, st, "infra", []int{443})

	spec := ansibleSpec("infra")
	spec.EgressPorts = []int{2222}
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if want := []int{443, 2222}; !slices.Equal(got.Args.EgressPorts, want) {
		t.Errorf("stored EgressPorts = %v, want %v (union of default and declared)", got.Args.EgressPorts, want)
	}
}

// TestEnqueueSourceWithoutDefaultUnchanged proves a source with no recorded
// default widens nothing: the run keeps exactly its declared set.
func TestEnqueueSourceWithoutDefaultUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)
	newEgressSource(t, st, "infra", nil)

	spec := ansibleSpec("infra")
	spec.EgressPorts = []int{2222}
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(got.Args.EgressPorts, []int{2222}) {
		t.Errorf("stored EgressPorts = %v, want [2222] (unchanged)", got.Args.EgressPorts)
	}
}

// TestEnqueueWidensOnlySourcedAnsible is the gate's negative half: a non-ansible
// job naming a source is never widened, and neither is an ansible job that names
// no source. Both keep exactly their declared set.
func TestEnqueueWidensOnlySourcedAnsible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)
	newEgressSource(t, st, "infra", []int{443})

	shell := shellSpec("/bin/true")
	shell.Source = "infra"
	shell.EgressPorts = []int{2222}
	shellJob, err := q.Enqueue(ctx, shell)
	if err != nil {
		t.Fatalf("enqueue shell: %v", err)
	}
	gotShell, err := st.GetJob(ctx, shellJob.ID)
	if err != nil {
		t.Fatalf("get shell job: %v", err)
	}
	if !slices.Equal(gotShell.Args.EgressPorts, []int{2222}) {
		t.Errorf("shell EgressPorts = %v, want [2222] (a non-ansible job is never widened)", gotShell.Args.EgressPorts)
	}

	anon := ansibleSpec("")
	anon.EgressPorts = []int{2222}
	anonJob, err := q.Enqueue(ctx, anon)
	if err != nil {
		t.Fatalf("enqueue sourceless ansible: %v", err)
	}
	gotAnon, err := st.GetJob(ctx, anonJob.ID)
	if err != nil {
		t.Fatalf("get sourceless job: %v", err)
	}
	if !slices.Equal(gotAnon.Args.EgressPorts, []int{2222}) {
		t.Errorf("sourceless EgressPorts = %v, want [2222] (no source, no widening)", gotAnon.Args.EgressPorts)
	}
}

// TestEnqueueMissingSourceUnchanged proves a named source that no longer exists
// neither errors nor substitutes another: a redrive of an old job whose source
// was deleted keeps working with the set its row already carries.
func TestEnqueueMissingSourceUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)

	spec := ansibleSpec("gone")
	spec.EgressPorts = []int{2222}
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue with missing source: %v", err)
	}

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(got.Args.EgressPorts, []int{2222}) {
		t.Errorf("stored EgressPorts = %v, want [2222] (missing source must not widen)", got.Args.EgressPorts)
	}
}

// TestEnqueueGatedUnionsSourceEgressDefault is EnqueueGated's half of the
// admission union: a gated ansible job naming a source inherits the default
// before it is parked, so the released grant equals the ungated one. Dropping
// the union from EnqueueGated fails this test.
func TestEnqueueGatedUnionsSourceEgressDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)
	newEgressSource(t, st, "infra", []int{443})

	spec := ansibleSpec("infra")
	spec.EgressPorts = []int{2222}
	gated, err := q.EnqueueGated(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue gated: %v", err)
	}

	got, err := st.GetJob(ctx, gated.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if want := []int{443, 2222}; !slices.Equal(got.Args.EgressPorts, want) {
		t.Errorf("stored EgressPorts = %v, want %v (union of default and declared)", got.Args.EgressPorts, want)
	}
}

// newBoundSource registers a source and, when binding is non-empty, records one
// pair under it and sets it as the source's default run-secret binding, through
// the real store.
func newBoundSource(t *testing.T, st *store.Store, name, binding string) {
	t.Helper()
	ctx := context.Background()
	src, err := st.CreateSource(ctx, store.Source{Name: name, Kind: store.SourceGit, Enabled: true})
	if err != nil {
		t.Fatalf("create source %q: %v", name, err)
	}
	if binding == "" {
		return
	}
	if _, err := st.AddRunSecret(ctx, store.RunSecret{
		Binding: binding, EnvName: "VAULT_PASSWORD", SecretName: "vault-pass",
	}); err != nil {
		t.Fatalf("add run secret %q: %v", binding, err)
	}
	if _, err := st.SetSourceRunSecrets(ctx, src.ID, binding); err != nil {
		t.Fatalf("set source %q run-secrets: %v", name, err)
	}
}

// TestEnqueueInheritsSourceRunSecrets is the admission hop for Decision 0024: an
// ansible job naming a source that passes no binding has the source default
// recorded on its row, ungated and gated alike, so whichever process claims it
// resolves the same env. Dropping the fill from either entry point fails this.
func TestEnqueueInheritsSourceRunSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)
	newBoundSource(t, st, "infra", "vault")

	queued, err := q.Enqueue(ctx, ansibleSpec("infra"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	gated, err := q.EnqueueGated(ctx, ansibleSpec("infra"))
	if err != nil {
		t.Fatalf("enqueue gated: %v", err)
	}
	for name, id := range map[string]int64{"queued": queued.ID, "gated": gated.ID} {
		got, err := st.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("get %s job: %v", name, err)
		}
		if got.Args.RunSecretBinding != "vault" {
			t.Errorf("%s RunSecretBinding = %q, want vault (the source default)", name, got.Args.RunSecretBinding)
		}
	}
}

// TestEnqueueExplicitRunSecretsWinOverSourceDefault: a run that names its own
// binding keeps it; the source default never replaces or merges with it.
func TestEnqueueExplicitRunSecretsWinOverSourceDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)
	newBoundSource(t, st, "infra", "vault")

	spec := ansibleSpec("infra")
	spec.RunSecretBinding = "other"
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Args.RunSecretBinding != "other" {
		t.Errorf("RunSecretBinding = %q, want other (explicit wins)", got.Args.RunSecretBinding)
	}
}

// TestEnqueueRunSecretsDefaultOnlyForSourcedAnsible is the negative half: a
// non-ansible job naming the source, an ansible job naming none, a source with
// no default, and a source that no longer exists all keep an empty binding.
func TestEnqueueRunSecretsDefaultOnlyForSourcedAnsible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q, st := newQueue(t, 1)
	newBoundSource(t, st, "infra", "vault")
	newBoundSource(t, st, "plain", "")

	shell := shellSpec("/bin/true")
	shell.Source = "infra"
	for name, spec := range map[string]queue.Spec{
		"shell":      shell,
		"sourceless": ansibleSpec(""),
		"no-default": ansibleSpec("plain"),
		"missing":    ansibleSpec("gone"),
	} {
		job, err := q.Enqueue(ctx, spec)
		if err != nil {
			t.Fatalf("enqueue %s: %v", name, err)
		}
		got, err := st.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatalf("get %s job: %v", name, err)
		}
		if got.Args.RunSecretBinding != "" {
			t.Errorf("%s RunSecretBinding = %q, want empty", name, got.Args.RunSecretBinding)
		}
	}
}
