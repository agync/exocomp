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
