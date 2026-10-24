package sched

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

const edgeInventory = `{"all": {"hosts": {"edge1": {"ansible_host": "10.0.0.1"}}}}`

func sourcedPlaySchedule(t *testing.T, st *store.Store, name, source string, argv ...string) store.Schedule {
	t.Helper()
	sc, err := st.CreateSchedule(t.Context(), store.NewSchedule{
		Name:     name,
		CronExpr: "@every 1h",
		Kind:     KindPlay,
		Args:     store.ScheduleArgs{Target: "tf:edge", Source: source, Argv: argv},
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	return sc
}

// A play schedule that names a source runs from that source's checkout: the
// resolver's confined playbook replaces the stored relative one, the checkout
// is the working directory, and the spec names the source so queue admission
// folds in its defaults. The standing inventory and the passthrough survive.
func TestPlayScheduleWithSourceRunsFromItsCheckout(t *testing.T) {
	st := newTestStore(t)
	materializedInventory(t, st, "edge", edgeInventory)
	sc := sourcedPlaySchedule(t, st, "converge", "infra", "plays/site.yml", "--check", "--diff")

	var gotSource, gotPlaybook string
	resolve := func(_ context.Context, source, playbook string) (string, string, bool, error) {
		gotSource, gotPlaybook = source, playbook
		return "/checkout/plays/site.yml", "/checkout", true, nil
	}
	s := New(st, &fakeEnqueuer{store: st}, testLogger(), WithSourceResolver(resolve))
	specs, err := s.buildSpecs(t.Context(), sc)
	if err != nil {
		t.Fatalf("buildSpecs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	spec := specs[0]

	if gotSource != "infra" || gotPlaybook != "plays/site.yml" {
		t.Errorf("resolver saw (%q, %q), want (infra, plays/site.yml)", gotSource, gotPlaybook)
	}
	if spec.Source != "infra" || spec.Dir != "/checkout" || !spec.DirReadOnly {
		t.Errorf("spec source/dir/readonly = %q/%q/%v, want infra, /checkout, read-only",
			spec.Source, spec.Dir, spec.DirReadOnly)
	}
	want := []string{executor.AnsibleBinary, "--check", "--diff", "/checkout/plays/site.yml"}
	if !reflect.DeepEqual(spec.Argv, want) {
		t.Errorf("argv = %v, want %v", spec.Argv, want)
	}
	if spec.Inventory != "tf:edge" || spec.Kind != executor.KindAnsible || spec.Trigger != Trigger {
		t.Errorf("spec = %+v, want the standing inventory on a scheduled ansible job", spec)
	}
}

// A schedule with no source is exactly what it was: the playbook is used as
// given, no source is named, and the resolver is never consulted.
func TestPlayScheduleWithoutSourceNeverResolves(t *testing.T) {
	st := newTestStore(t)
	materializedInventory(t, st, "edge", edgeInventory)
	sc := playSchedule(t, st, "converge", "tf:edge")

	resolve := func(context.Context, string, string) (string, string, bool, error) {
		t.Error("the resolver ran for a schedule that names no source")
		return "", "", false, nil
	}
	s := New(st, &fakeEnqueuer{store: st}, testLogger(), WithSourceResolver(resolve))
	specs, err := s.buildSpecs(t.Context(), sc)
	if err != nil {
		t.Fatalf("buildSpecs: %v", err)
	}
	spec := specs[0]
	if spec.Source != "" || spec.Dir != "" || spec.DirReadOnly {
		t.Errorf("spec source/dir/readonly = %q/%q/%v, want none", spec.Source, spec.Dir, spec.DirReadOnly)
	}
	if got := spec.Argv[len(spec.Argv)-1]; got != "/srv/play/converge.yml" {
		t.Errorf("argv = %v, want the playbook path as stored", spec.Argv)
	}
}

// The fail-closed rule extends to the source: one that cannot be resolved, or a
// scheduler built with no resolver, enqueues nothing rather than falling back
// to a sourceless run that would skip the defaults the schedule asked for.
func TestPlayScheduleSourceFailsClosed(t *testing.T) {
	st := newTestStore(t)
	materializedInventory(t, st, "edge", edgeInventory)
	sc := sourcedPlaySchedule(t, st, "converge", "infra", "plays/site.yml")

	broken := func(context.Context, string, string) (string, string, bool, error) {
		return "", "", false, errors.New("source has hooks")
	}
	for name, opts := range map[string][]Option{
		"resolver error": {WithSourceResolver(broken)},
		"no resolver":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			q := &fakeEnqueuer{store: st}
			s := New(st, q, testLogger(), opts...)
			if _, err := s.buildSpecs(t.Context(), sc); err == nil {
				t.Fatal("buildSpecs built a spec for an unresolvable source")
			}
			if err := s.Start(t.Context()); err != nil {
				t.Fatalf("start: %v", err)
			}
			t.Cleanup(s.Stop)
			s.fire(sc)
			if specs := q.Specs(); len(specs) != 0 {
				t.Fatalf("fire enqueued %d spec(s) for an unresolvable source", len(specs))
			}
		})
	}
}

// End to end through the real service resolution and the real queue: a fire of a
// source-named play lands a job that runs from the checkout and carries the
// source's default egress, unioned with nothing the schedule declares, and its
// default run-secret binding. A sourceless fire of the same play gets neither.
func TestPlayScheduleFireInheritsSourceDefaults(t *testing.T) {
	ctx := t.Context()
	st := newTestStore(t)
	materializedInventory(t, st, "edge", edgeInventory)

	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "plays"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "plays", "site.yml"), []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	src, err := st.CreateSource(ctx, store.Source{Name: "infra", Kind: store.SourcePath, URI: checkout, Enabled: true})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := st.SetSourceEgressPorts(ctx, src.ID, []int{443}); err != nil {
		t.Fatalf("set egress: %v", err)
	}
	if _, err := st.AddRunSecret(ctx, store.RunSecret{
		Binding: "tree-vault", EnvName: "VAULT_PASSWORD", SecretName: "tree-vault-pass",
	}); err != nil {
		t.Fatalf("add run secret: %v", err)
	}
	if _, err := st.SetSourceRunSecrets(ctx, src.ID, "tree-vault"); err != nil {
		t.Fatalf("set run-secrets: %v", err)
	}

	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers: 1, PollInterval: 10 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	svc := service.New(st, t.TempDir(), nil)
	s := New(st, q, testLogger(), WithSourceResolver(svc.ResolvePlainSourcePlay))
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)

	sourced := sourcedPlaySchedule(t, st, "sourced", "infra", "plays/site.yml")
	plain := playSchedule(t, st, "plain", "tf:edge")
	s.fire(sourced)
	s.fire(plain)

	jobFor := func(sc store.Schedule) store.Job {
		t.Helper()
		fresh, err := st.GetScheduleByID(ctx, sc.ID)
		if err != nil || fresh.LastJobID == nil {
			t.Fatalf("schedule %s did not record a job: %+v, %v", sc.Name, fresh, err)
		}
		job, err := st.GetJob(ctx, *fresh.LastJobID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		return job
	}

	got := jobFor(sourced).Args
	if got.Source != "infra" || got.Dir != checkout || !got.DirReadOnly {
		t.Errorf("sourced job source/dir/readonly = %q/%q/%v, want infra, %q, read-only",
			got.Source, got.Dir, got.DirReadOnly, checkout)
	}
	if !reflect.DeepEqual(got.EgressPorts, []int{443}) {
		t.Errorf("sourced egress = %v, want the source default [443]", got.EgressPorts)
	}
	if got.RunSecretBinding != "tree-vault" {
		t.Errorf("sourced run-secret binding = %q, want tree-vault", got.RunSecretBinding)
	}
	if want := filepath.Join(checkout, "plays", "site.yml"); !strings.HasSuffix(strings.Join(got.Argv, " "), want) {
		t.Errorf("sourced argv = %v, want the playbook rebased onto the checkout", got.Argv)
	}

	base := jobFor(plain).Args
	if base.Source != "" || base.Dir != "" || len(base.EgressPorts) != 0 || base.RunSecretBinding != "" {
		t.Errorf("sourceless job = %+v, want no source, dir, egress or binding", base)
	}
}

// The exported builder a Run now uses and the scheduler's fire build agree for a
// source-named play, so a manual run carries the same source, directory and
// playbook as the cron fire.
func TestSpecsMatchesFireBuildForASourcedPlay(t *testing.T) {
	st := newTestStore(t)
	materializedInventory(t, st, "edge", edgeInventory)
	sc := sourcedPlaySchedule(t, st, "converge", "infra", "plays/site.yml", "--check")
	resolve := func(context.Context, string, string) (string, string, bool, error) {
		return "/checkout/plays/site.yml", "/checkout", true, nil
	}

	fire, err := New(st, &fakeEnqueuer{store: st}, testLogger(), WithSourceResolver(resolve)).buildSpecs(t.Context(), sc)
	if err != nil {
		t.Fatalf("buildSpecs: %v", err)
	}
	direct, err := Specs(t.Context(), st, testLogger(), sc, Trigger, resolve)
	if err != nil {
		t.Fatalf("Specs: %v", err)
	}
	if !reflect.DeepEqual(fire, direct) {
		t.Fatalf("Specs = %+v, fire build = %+v", direct, fire)
	}
	manual, err := Specs(t.Context(), st, testLogger(), sc, "run-now", resolve)
	if err != nil || len(manual) != 1 || manual[0].Trigger != "run-now" || manual[0].Source != "infra" {
		t.Fatalf("run-now spec = %+v, %v; want a run-now trigger that still names infra", manual, err)
	}
}
