package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// captureCanary is the value a play "fetches" into the capture dir. It is the
// sealed material and must never reach argv, an env entry, or a job event.
const captureCanary = "exocomp-ansible-capture-7b2e4c"

// recSink records every event a run emits, safe for the concurrent Emit the
// driver's observers do.
type recSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *recSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *recSink) all() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// captureFake is a faithful stand-in for ansible-playbook: it reads the capture
// dir the driver exported through the extra-vars file — exactly where a real
// play's `fetch` writes — and drops the declared files there, then exits 0. The
// argv and env it saw are kept so a test can prove the canary reached neither.
type captureFake struct {
	produce map[string]string
	gotArgv []string
	gotEnv  []string
}

func (d *captureFake) Run(_ context.Context, spec Spec, sink EventSink) (Result, error) {
	d.gotArgv = append([]string(nil), spec.Argv...)
	d.gotEnv = append([]string(nil), spec.Env...)
	dir, err := captureDirFromExtraVars(spec.Argv)
	if err != nil {
		return Result{}, err
	}
	for name, content := range d.produce {
		if dir == "" {
			return Result{}, os.ErrInvalid
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return Result{}, err
		}
	}
	sink.Emit(Event{Ts: time.Now().UTC(), Level: LevelInfo, Msg: "PLAY RECAP"})
	return Result{ExitCode: 0}, nil
}

// captureDirFromExtraVars extracts the capture dir the driver staged into the
// run's extra-vars file, the way a play reads {{ exocomp_capture_dir }}.
func captureDirFromExtraVars(argv []string) (string, error) {
	var path string
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "--extra-vars" && strings.HasPrefix(argv[i+1], "@") {
			path = strings.TrimPrefix(argv[i+1], "@")
		}
	}
	if path == "" {
		return "", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var vars map[string]string
	if err := json.Unmarshal(body, &vars); err != nil {
		return "", err
	}
	return vars[AnsibleCaptureDirVar], nil
}

func newSealFixture(t *testing.T) (*secrets.Envelope, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	env, _, err := secrets.Init(filepath.Join(dir, "envelope"), nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return env, st
}

// runCaptureDriver runs the ansible driver with a faked inner process that
// produces `produce` in the capture dir, and returns the sink and the fake.
func runCaptureDriver(t *testing.T, env *secrets.Envelope, st *store.Store, workflow string, captures []AnsibleCapture, produce map[string]string) (*recSink, *captureFake) {
	t.Helper()
	drv := NewAnsible(AnsibleOptions{
		Binary:         "/bin/true", // never executed: the inner driver is faked
		PrivateDirRoot: t.TempDir(),
		Captures:       captures,
		OutputSealer:   AnsibleOutputSealer{Env: env, Store: st, Workflow: workflow},
	})
	fake := &captureFake{produce: produce}
	drv.inner = fake

	sink := &recSink{}
	res, err := drv.Run(context.Background(), Spec{
		Kind: KindAnsible,
		Argv: AnsibleArgv("/p/site.yml", ""),
	}, sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}
	return sink, fake
}

func findAnsibleCapture(t *testing.T, sink *recSink) AnsibleCaptureResult {
	t.Helper()
	for _, e := range sink.all() {
		if e.RawJSON == "" {
			continue
		}
		var env struct {
			Capture *AnsibleCaptureResult `json:"ansible_capture"`
		}
		if json.Unmarshal([]byte(e.RawJSON), &env) == nil && env.Capture != nil {
			return *env.Capture
		}
	}
	t.Fatalf("no ansible_capture event in run")
	return AnsibleCaptureResult{}
}

func assertCanaryAbsent(t *testing.T, sink *recSink, fake *captureFake) {
	t.Helper()
	for _, e := range sink.all() {
		if strings.Contains(e.Msg, captureCanary) || strings.Contains(e.RawJSON, captureCanary) {
			t.Fatalf("event carried the capture canary: %q / %q", e.Msg, e.RawJSON)
		}
	}
	if joined := strings.Join(fake.gotArgv, " "); strings.Contains(joined, captureCanary) {
		t.Fatalf("argv carried the capture canary: %q", joined)
	}
	if joined := strings.Join(fake.gotEnv, "\x00"); strings.Contains(joined, captureCanary) {
		t.Fatalf("env carried the capture canary")
	}
}

// TestAnsibleCaptureSealsAndBinds is the Phase-1 gate: a play that produces a
// declared capture gets it sealed under ansible-output with the right binding
// and env name, the value opens back exactly, and the canary reaches neither
// argv, env, nor any event.
func TestAnsibleCaptureSealsAndBinds(t *testing.T) {
	env, st := newSealFixture(t)
	ctx := context.Background()
	const wf = "boot-1"

	sink, fake := runCaptureDriver(t, env, st, wf,
		[]AnsibleCapture{{Name: "kubeconfig", File: "admin.conf"}},
		map[string]string{"admin.conf": captureCanary})

	// The sealed row is stored under the deterministic name, kind ansible-output,
	// and opens back to exactly what the play produced.
	secName := AnsibleOutputSecretName(wf, "kubeconfig")
	row, err := st.GetSecret(ctx, secName)
	if err != nil {
		t.Fatalf("sealed capture %q not stored: %v", secName, err)
	}
	if row.Kind != SecretKindAnsibleOutput {
		t.Errorf("kind = %q, want %q", row.Kind, SecretKindAnsibleOutput)
	}
	if row.AAD != secName+"|"+SecretKindAnsibleOutput {
		t.Errorf("AAD = %q, want %q", row.AAD, secName+"|"+SecretKindAnsibleOutput)
	}
	pt, err := env.OpenSecret(secrets.SealedSecret{
		Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open sealed capture: %v", err)
	}
	if string(pt) != captureCanary {
		t.Fatalf("sealed value = %q, want the canary", pt)
	}

	// The env binding maps KUBECONFIG under ab:<workflow> to that secret.
	pairs, err := st.RunSecretsByBinding(ctx, AnsibleOutputBinding(wf))
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if len(pairs) != 1 || pairs[0].EnvName != "KUBECONFIG" || pairs[0].SecretName != secName {
		t.Fatalf("binding = %+v, want KUBECONFIG -> %s", pairs, secName)
	}

	// The capture event names what it sealed, and nothing carries the value.
	if got := findAnsibleCapture(t, sink); len(got.Sealed) != 1 || got.Sealed[0] != "kubeconfig" {
		t.Errorf("capture summary = %+v, want sealed [kubeconfig]", got)
	}
	assertCanaryAbsent(t, sink, fake)
}

// TestAnsibleCaptureIdempotent proves a re-run reseals in place: one secret row,
// one binding pair, no duplicates.
func TestAnsibleCaptureIdempotent(t *testing.T) {
	env, st := newSealFixture(t)
	ctx := context.Background()
	const wf = "boot-2"
	captures := []AnsibleCapture{{Name: "kubeconfig", File: "admin.conf"}}

	runCaptureDriver(t, env, st, wf, captures, map[string]string{"admin.conf": captureCanary})
	runCaptureDriver(t, env, st, wf, captures, map[string]string{"admin.conf": captureCanary + "-rotated"})

	// A single row survives, holding the latest value.
	secName := AnsibleOutputSecretName(wf, "kubeconfig")
	all, err := st.ListSecrets(ctx)
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	var n int
	for _, s := range all {
		if s.Name == secName {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("secret row count = %d, want 1 (idempotent reseal)", n)
	}
	row, _ := st.GetSecret(ctx, secName)
	pt, err := env.OpenSecret(secrets.SealedSecret{
		Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(pt) != captureCanary+"-rotated" {
		t.Fatalf("resealed value = %q, want the rotated canary", pt)
	}

	// And a single binding pair, not two.
	pairs, err := st.RunSecretsByBinding(ctx, AnsibleOutputBinding(wf))
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("binding pair count = %d, want 1 (no duplicate)", len(pairs))
	}
}

// TestAnsibleCaptureNonPOSIXNameSkipped proves a capture whose name cannot fold
// to a POSIX env identifier is sealed but not bound — skipped with a reason, not
// mangled into an env var the operator never wrote.
func TestAnsibleCaptureNonPOSIXNameSkipped(t *testing.T) {
	env, st := newSealFixture(t)
	ctx := context.Background()
	const wf = "boot-3"

	sink, _ := runCaptureDriver(t, env, st, wf,
		[]AnsibleCapture{{Name: "kube-config", File: "admin.conf"}},
		map[string]string{"admin.conf": captureCanary})

	// It is still sealed (the value is stored) ...
	if _, err := st.GetSecret(ctx, AnsibleOutputSecretName(wf, "kube-config")); err != nil {
		t.Fatalf("non-POSIX capture not sealed: %v", err)
	}
	// ... but nothing is bound, and no mangled env name was invented.
	pairs, err := st.RunSecretsByBinding(ctx, AnsibleOutputBinding(wf))
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("non-POSIX name produced a binding: %+v", pairs)
	}
	got := findAnsibleCapture(t, sink)
	if len(got.Unbound) != 1 || !strings.Contains(got.Unbound[0], "POSIX identifier") {
		t.Fatalf("capture summary = %+v, want an unbound reason", got)
	}
}

// TestAnsibleCaptureMissingTargetReported proves a declared target the play did
// not produce is a report, not a run failure.
func TestAnsibleCaptureMissingTargetReported(t *testing.T) {
	env, st := newSealFixture(t)
	ctx := context.Background()
	const wf = "boot-4"

	sink, _ := runCaptureDriver(t, env, st, wf,
		[]AnsibleCapture{{Name: "kubeconfig", File: "admin.conf"}},
		nil) // the play produces nothing

	got := findAnsibleCapture(t, sink)
	if len(got.Sealed) != 0 || len(got.Missing) != 1 || got.Missing[0] != "kubeconfig" {
		t.Fatalf("capture summary = %+v, want missing [kubeconfig]", got)
	}
	if _, err := st.GetSecret(ctx, AnsibleOutputSecretName(wf, "kubeconfig")); err == nil {
		t.Fatalf("a missing target must not seal a row")
	}
}

// TestAnsibleOutputSealerEnvNameCollision proves the sealer refuses to repoint
// an env name already bound elsewhere: the second capture folding to the same
// env name is sealed but reported unbound.
func TestAnsibleOutputSealerEnvNameCollision(t *testing.T) {
	env, st := newSealFixture(t)
	ctx := context.Background()
	sealer := AnsibleOutputSealer{Env: env, Store: st, Workflow: "boot-5"}

	if reason, err := sealer.SealOutput(ctx, "kubeconfig", []byte("first")); err != nil || reason != "" {
		t.Fatalf("first seal reason=%q err=%v, want bound", reason, err)
	}
	// "KUBECONFIG" folds to the same env name; it is sealed under its own row but
	// must not silently repoint the existing binding.
	reason, err := sealer.SealOutput(ctx, "KUBECONFIG", []byte("second"))
	if err != nil {
		t.Fatalf("second seal: %v", err)
	}
	if !strings.Contains(reason, "already binds") {
		t.Fatalf("collision reason = %q, want an already-binds report", reason)
	}
	pairs, err := st.RunSecretsByBinding(ctx, AnsibleOutputBinding("boot-5"))
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if len(pairs) != 1 || pairs[0].SecretName != AnsibleOutputSecretName("boot-5", "kubeconfig") {
		t.Fatalf("binding = %+v, want the first capture kept", pairs)
	}
}

// TestAnsibleOutputEnvName proves folding refuses non-identifiers rather than
// substituting characters.
func TestAnsibleOutputEnvName(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"kubeconfig":  {"KUBECONFIG", true},
		"kube_token":  {"KUBE_TOKEN", true},
		"kube-config": {"", false},
		"kube.config": {"", false},
		"":            {"", false},
	}
	for in, want := range cases {
		got, ok := AnsibleOutputEnvName(in)
		if got != want.want || ok != want.ok {
			t.Errorf("AnsibleOutputEnvName(%q) = %q,%v want %q,%v", in, got, ok, want.want, want.ok)
		}
	}
}
