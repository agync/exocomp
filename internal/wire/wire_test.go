package wire_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func ptrInt(n int) *int { return &n }

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func atp(s string) *time.Time { t := at(s); return &t }

// The golden strings pin the exact bytes the marshaller emits. They are the
// contract: a change here is a schema change and must bump SchemaVersion.
func TestGoldenJobList(t *testing.T) {
	jobs := []store.Job{{
		ID:             7,
		Runbook:        "web",
		Kind:           "tofu.plan",
		Trigger:        "manual",
		State:          store.JobState("succeeded"),
		ExitCode:       ptrInt(0),
		ConcurrencyKey: "workspace:web",
		QueuedAt:       at("2026-01-02T03:04:05Z"),
		StartedAt:      atp("2026-01-02T03:04:06Z"),
		FinishedAt:     atp("2026-01-02T03:04:10Z"),
	}}

	got, err := wire.MarshalSuccess(wire.KindJobList, wire.NewJobList(jobs))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"job.list","data":[{"id":7,"runbook":"web","kind":"tofu.plan","trigger":"manual","state":"succeeded","exit_code":0,"concurrency_key":"workspace:web","queued_at":"2026-01-02T03:04:05Z","started_at":"2026-01-02T03:04:06Z","finished_at":"2026-01-02T03:04:10Z"}]}` + "\n"
	if string(got) != want {
		t.Fatalf("job list bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestGoldenJobPrivileged pins the additive privileged field: it appears only
// on a job that ran unsandboxed, beside the source display fact, and its
// absence on a sandboxed job leaves TestGoldenJobList's bytes unchanged. An
// additive optional field is not a schema break — the progress field was added
// the same way (internal/wire/progress.go) without a SchemaVersion bump.
func TestGoldenJobPrivileged(t *testing.T) {
	job := store.Job{
		ID:       7,
		Runbook:  "web",
		Kind:     "ansible",
		Trigger:  "manual",
		State:    store.JobState("succeeded"),
		QueuedAt: at("2026-01-02T03:04:05Z"),
		Args:     store.JobArgs{Source: "tree", Privileged: true},
	}
	got, err := wire.MarshalSuccess(wire.KindJob, wire.NewJobView(job))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"job.get","data":{"id":7,"runbook":"web","kind":"ansible","trigger":"manual","source":"tree","privileged":true,"state":"succeeded","queued_at":"2026-01-02T03:04:05Z"}}` + "\n"
	if string(got) != want {
		t.Fatalf("privileged job bytes drift:\n got: %s\nwant: %s", got, want)
	}

	// A sandboxed job omits the field entirely (omitempty), so the quiet
	// default stays a tiny document.
	job.Args.Privileged = false
	got, err = wire.MarshalSuccess(wire.KindJob, wire.NewJobView(job))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "privileged") {
		t.Fatalf("sandboxed job must omit privileged: %s", got)
	}
}

// TestGoldenSourceEgressDefault pins the additive extra_egress_ports field on a
// source view: it appears only when the operator set a source-scoped default
// (Decision 0023) and its absence leaves the pre-existing source bytes
// unchanged, so the pinned schema_version stays at 1 — the same additive rule
// the earlier privileged and progress fields followed.
func TestGoldenSourceEgressDefault(t *testing.T) {
	src := store.Source{
		ID:               7,
		Name:             "tree",
		Kind:             "path",
		Enabled:          true,
		ExtraEgressPorts: []int{8080, 9000},
		CreatedAt:        at("2026-01-02T03:04:05Z"),
	}
	got, err := wire.MarshalSuccess(wire.KindSourceList, wire.NewSourceList([]store.Source{src}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"source.list","data":[{"id":7,"name":"tree","kind":"path","enabled":true,"extra_egress_ports":[8080,9000],"created_at":"2026-01-02T03:04:05Z"}]}` + "\n"
	if string(got) != want {
		t.Fatalf("source egress default bytes drift:\n got: %s\nwant: %s", got, want)
	}

	// A source with no operator default omits the field entirely (omitempty),
	// so the default posture stays a tiny document.
	src.ExtraEgressPorts = nil
	got, err = wire.MarshalSuccess(wire.KindSourceList, wire.NewSourceList([]store.Source{src}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "extra_egress_ports") {
		t.Fatalf("source without a default must omit extra_egress_ports: %s", got)
	}
}

// TestGoldenSourceRunSecretsDefault pins the additive default_run_secrets field
// on a source view (Decision 0024): the binding name appears only when the
// operator set one, and its absence leaves the pre-existing bytes unchanged.
func TestGoldenSourceRunSecretsDefault(t *testing.T) {
	src := store.Source{
		ID:                7,
		Name:              "tree",
		Kind:              "path",
		Enabled:           true,
		DefaultRunSecrets: "tree-vault",
		CreatedAt:         at("2026-01-02T03:04:05Z"),
	}
	got, err := wire.MarshalSuccess(wire.KindSourceList, wire.NewSourceList([]store.Source{src}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"source.list","data":[{"id":7,"name":"tree","kind":"path","enabled":true,"default_run_secrets":"tree-vault","created_at":"2026-01-02T03:04:05Z"}]}` + "\n"
	if string(got) != want {
		t.Fatalf("source run-secrets default bytes drift:\n got: %s\nwant: %s", got, want)
	}

	src.DefaultRunSecrets = ""
	got, err = wire.MarshalSuccess(wire.KindSourceList, wire.NewSourceList([]store.Source{src}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "default_run_secrets") {
		t.Fatalf("source without a default must omit default_run_secrets: %s", got)
	}
}

func TestGoldenEmptyList(t *testing.T) {
	got, err := wire.MarshalSuccess(wire.KindJobList, wire.NewJobList(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"job.list","data":[]}` + "\n"
	if string(got) != want {
		t.Fatalf("empty list must render data:[] not null:\n got: %s\nwant: %s", got, want)
	}
}

func TestGoldenError(t *testing.T) {
	var buf bytes.Buffer
	if err := wire.WriteError(&buf, store.ErrNotFound); err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"error","error":{"code":"not_found",` +
		`"message":"store: not found",` +
		`"hint":"check the id or name; list the collection to see what exists"}}` + "\n"
	if buf.String() != want {
		t.Fatalf("error bytes drift:\n got: %s\nwant: %s", buf.String(), want)
	}
}

// TestSecretViewHasNoSealedFields is a structural guard: the projection type
// must expose no field that could carry ciphertext, nonce, or AAD.
func TestSecretViewHasNoSealedFields(t *testing.T) {
	rows := []store.SecretRow{{
		ID:         3,
		Name:       "vault/ansible",
		Kind:       "password",
		Nonce:      []byte("NONCE-MUST-NOT-LEAK"),
		Ciphertext: []byte("CIPHER-MUST-NOT-LEAK"),
		AAD:        "AAD-MUST-NOT-LEAK",
		Source:     "generated",
		CreatedAt:  at("2026-01-02T03:04:05Z"),
	}}
	got, err := wire.MarshalSuccess(wire.KindSecretList, wire.NewSecretList(rows))
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"NONCE-MUST-NOT-LEAK", "CIPHER-MUST-NOT-LEAK", "AAD-MUST-NOT-LEAK", "nonce", "ciphertext", "aad"} {
		if strings.Contains(string(got), banned) {
			t.Fatalf("secret list leaked %q: %s", banned, got)
		}
	}
	want := `{"schema_version":1,"kind":"secret.list","data":[{"id":3,"name":"vault/ansible","kind":"password","source":"generated","created_at":"2026-01-02T03:04:05Z"}]}` + "\n"
	if string(got) != want {
		t.Fatalf("secret metadata bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestGoldenDoctorHealth pins the doctor health envelope, including a
// not-fully-healthy document so the warn/fail/unknown path is covered, not
// just the all-ok case.
func TestGoldenDoctorHealth(t *testing.T) {
	entries := wire.HealthView{
		{Name: "data_dir", Status: wire.HealthOK, Detail: "mode 0700 — writable"},
		{Name: "secrets_protection", Status: wire.HealthFail, Detail: "host key: plaintext-in-datadir; memory lock: locked"},
		{Name: "optional_tools", Status: wire.HealthWarn, Detail: "missing: restic, age"},
		{Name: "self_observability", Status: wire.HealthUnknown, Detail: "metrics: daemon not running (no address file); dead-man: not configured — no external heartbeat wired yet; hold: none active"},
		{Name: "litestream", Status: wire.HealthUnknown, Detail: "binary not installed (optional); no replica target configured"},
	}

	got, err := wire.MarshalSuccess(wire.KindDoctor, entries)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"doctor","data":[` +
		`{"name":"data_dir","status":"ok","detail":"mode 0700 — writable"},` +
		`{"name":"secrets_protection","status":"fail","detail":"host key: plaintext-in-datadir; memory lock: locked"},` +
		`{"name":"optional_tools","status":"warn","detail":"missing: restic, age"},` +
		`{"name":"self_observability","status":"unknown","detail":"metrics: daemon not running (no address file); dead-man: not configured — no external heartbeat wired yet; hold: none active"},` +
		`{"name":"litestream","status":"unknown","detail":"binary not installed (optional); no replica target configured"}` +
		`]}` + "\n"
	if string(got) != want {
		t.Fatalf("doctor health bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestGoldenDoctorHealthEmpty pins the empty case: data must render "[]", not
// null, exactly like every other list projection.
func TestGoldenDoctorHealthEmpty(t *testing.T) {
	got, err := wire.MarshalSuccess(wire.KindDoctor, wire.HealthView{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"doctor","data":[]}` + "\n"
	if string(got) != want {
		t.Fatalf("empty doctor health bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

// A play schedule's source is an identifier and travels in the projection; a
// schedule with none leaves the key out, so existing consumers see no change.
func TestScheduleViewCarriesSource(t *testing.T) {
	views := wire.NewScheduleList([]store.Schedule{
		{ID: 1, Name: "sourced", CronExpr: "@every 1h", Kind: "play", Enabled: true,
			Args: store.ScheduleArgs{Argv: []string{"site.yml"}, Source: "infra"}},
		{ID: 2, Name: "plain", CronExpr: "@every 1h", Kind: "play", Enabled: true,
			Args: store.ScheduleArgs{Argv: []string{"/srv/site.yml"}}},
	})
	if views[0].Source != "infra" || views[1].Source != "" {
		t.Fatalf("sources = %q, %q; want infra and empty", views[0].Source, views[1].Source)
	}
	var buf bytes.Buffer
	if err := wire.WriteSuccess(&buf, wire.KindScheduleList, views); err != nil {
		t.Fatalf("WriteSuccess: %v", err)
	}
	out := buf.String()
	if got := strings.Count(out, `"source"`); got != 1 {
		t.Fatalf("source keys = %d in %s, want 1 (omitted when empty)", got, out)
	}
	if !strings.Contains(out, `"source":"infra"`) {
		t.Fatalf("schedule list = %s, want the source named", out)
	}
}

func TestExitCodeTable(t *testing.T) {
	cases := []struct {
		code string
		exit int
	}{
		{wire.CodeNotFound, wire.ExitNotFound},
		{wire.CodeUnauthenticated, wire.ExitAuth},
		{wire.CodeForbidden, wire.ExitAuth},
		{wire.CodeConflict, wire.ExitConflict},
		{wire.CodeConcurrencyHeld, wire.ExitConflict},
		{wire.CodeStalePlan, wire.ExitConflict},
		{wire.CodeKDFBusy, wire.ExitBusy},
		{wire.CodeRemoteUnsupported, wire.ExitFailure},
		{wire.CodeInternal, wire.ExitFailure},
	}
	for _, c := range cases {
		if got := wire.ExitForCode(c.code); got != c.exit {
			t.Errorf("ExitForCode(%q) = %d, want %d", c.code, got, c.exit)
		}
	}
	if wire.ExitFor(nil) != wire.ExitOK {
		t.Errorf("ExitFor(nil) must be ExitOK")
	}
	if wire.ExitFor(store.ErrNotFound) != wire.ExitNotFound {
		t.Errorf("ExitFor(ErrNotFound) must be ExitNotFound")
	}
}
