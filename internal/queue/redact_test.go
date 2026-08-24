package queue_test

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

func quietOptions() queue.Options {
	return queue.Options{
		Workers:      1,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// leakyDriver emits secret material on every channel an event carries.
type leakyDriver struct{ secret string }

func (d leakyDriver) Run(_ context.Context, _ executor.Spec, sink executor.EventSink) (executor.Result, error) {
	sink.Emit(executor.Event{Level: executor.LevelInfo, Msg: "password is " + d.secret})
	sink.Emit(executor.Event{
		Level:   executor.LevelInfo,
		Msg:     "structured",
		RawJSON: `{"password":"` + d.secret + `"}`,
	})
	return executor.Result{}, nil
}

func eventText(t *testing.T, st *store.Store, jobID int64) string {
	t.Helper()
	events, err := st.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("list job events: %v", err)
	}
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e.Msg)
		b.WriteByte('\n')
		b.WriteString(e.RawJSON)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestWithRedactorScrubsPersistedEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const secret = "hunter2-not-in-the-database"
	st := newStore(t)
	q := queue.New(st, leakyDriver{secret: secret}, quietOptions(),
		queue.WithRedactor(secrets.NewRedactor([][]byte{[]byte(secret)})))

	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook: "test", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, q)

	text := eventText(t, st, job.ID)
	if strings.Contains(text, secret) {
		t.Errorf("secret survived into job_event:\n%s", text)
	}
	if strings.Count(text, secrets.Marker) != 2 {
		t.Errorf("want the marker in both the message and the payload:\n%s", text)
	}
}

func TestWithoutRedactorEventsAreVerbatim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const secret = "hunter2-not-in-the-database"
	st := newStore(t)
	q := queue.New(st, leakyDriver{secret: secret}, quietOptions())

	job, err := q.Enqueue(ctx, queue.Spec{
		Runbook: "test", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, q)

	// The counterpart of the redaction test: without the option the same
	// events do land verbatim, so the assertion above is not vacuous.
	if text := eventText(t, st, job.ID); !strings.Contains(text, secret) {
		t.Errorf("the driver's output never reached job_event:\n%s", text)
	}
}

// TestVaultedPlaybookLeavesNoSecretInEvents is the ansible gate: a real
// playbook decrypts a vaulted variable with the envelope-derived password and
// prints it, and no job_event row holds the plaintext.
func TestVaultedPlaybookLeavesNoSecretInEvents(t *testing.T) {
	for _, bin := range []string{executor.AnsibleBinary, "ansible-vault"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed: %v", bin, err)
		}
	}
	t.Setenv("HOME", t.TempDir())

	env, _, err := secrets.Init(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("init envelope: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })

	derived := env.Derive("ansible-vault", "homelab")
	password := []byte(base64.StdEncoding.EncodeToString(derived))
	const secret = "vaulted-canary-c0ffee"

	dir := t.TempDir()
	playbook := writeVaultedPlaybook(t, dir, string(password), secret)

	privateRoot := t.TempDir()
	st := newStore(t)
	q := queue.New(st, executor.NewAnsible(executor.AnsibleOptions{
		VaultPassword:  password,
		PrivateDirRoot: privateRoot,
	}), quietOptions(), queue.WithRedactor(secrets.NewRedactor([][]byte{
		derived, password, []byte(secret),
	})))

	job, err := q.Enqueue(context.Background(), queue.Spec{
		Runbook: "playbook",
		Kind:    executor.KindAnsible,
		Trigger: "manual",
		Argv:    executor.AnsibleArgv(playbook, ""),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, q)

	final, err := st.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	text := eventText(t, st, job.ID)
	if final.State != store.StateSucceeded {
		t.Fatalf("state = %s, want succeeded:\n%s", final.State, text)
	}
	if final.ExitCode == nil || *final.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", final.ExitCode)
	}

	if strings.Contains(text, secret) {
		t.Errorf("the vaulted value reached job_event:\n%s", text)
	}
	if strings.Contains(text, string(password)) {
		t.Errorf("the vault password reached job_event:\n%s", text)
	}
	// Without the marker the run could have failed to decrypt at all, which
	// would make the absence of the plaintext meaningless.
	if !strings.Contains(text, secrets.Marker) {
		t.Errorf("no redaction marker: the vaulted value never printed:\n%s", text)
	}

	left, err := os.ReadDir(privateRoot)
	if err != nil {
		t.Fatalf("read private dir root: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("vault password directory survived the run: %v", left)
	}
}

func TestFailingPlaybookFailsTheJob(t *testing.T) {
	if _, err := exec.LookPath(executor.AnsibleBinary); err != nil {
		t.Skipf("%s not installed: %v", executor.AnsibleBinary, err)
	}
	t.Setenv("HOME", t.TempDir())

	playbook := filepath.Join(t.TempDir(), "fail.yml")
	body := `- name: exocomp failure path
  hosts: localhost
  gather_facts: false
  tasks:
    - name: fail on purpose
      ansible.builtin.command: /bin/true
      changed_when: false
      failed_when: true
`
	if err := os.WriteFile(playbook, []byte(body), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	st := newStore(t)
	q := queue.New(st, executor.NewAnsible(executor.AnsibleOptions{
		PrivateDirRoot: t.TempDir(),
	}), quietOptions())

	job, err := q.Enqueue(context.Background(), queue.Spec{
		Runbook: "playbook",
		Kind:    executor.KindAnsible,
		Trigger: "manual",
		Argv:    executor.AnsibleArgv(playbook, ""),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, q)

	final, err := st.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if final.State != store.StateFailed {
		t.Errorf("state = %s, want failed:\n%s", final.State, eventText(t, st, job.ID))
	}
	if final.ExitCode == nil || *final.ExitCode == 0 {
		t.Errorf("exit code = %v, want non-zero", final.ExitCode)
	}
}

// writeVaultedPlaybook builds a playbook whose only variable is encrypted by
// the ansible-vault CLI, so the run decrypts a real vault payload.
func writeVaultedPlaybook(t *testing.T, dir, password, secret string) string {
	t.Helper()

	pwFile := filepath.Join(dir, "vault-pass")
	if err := os.WriteFile(pwFile, []byte(password+"\n"), 0o600); err != nil {
		t.Fatalf("write vault password: %v", err)
	}
	vars := filepath.Join(dir, "secret.yml")
	if err := os.WriteFile(vars, []byte("exocomp_canary: "+secret+"\n"), 0o600); err != nil {
		t.Fatalf("write vars: %v", err)
	}
	cmd := exec.Command("ansible-vault", "encrypt",
		"--vault-id", executor.DefaultVaultID+"@"+pwFile, vars)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ansible-vault encrypt: %v: %s", err, out)
	}
	if err := os.Remove(pwFile); err != nil {
		t.Fatalf("remove vault password: %v", err)
	}

	playbook := filepath.Join(dir, "site.yml")
	body := `- name: exocomp vault round trip
  hosts: localhost
  gather_facts: false
  vars_files:
    - secret.yml
  tasks:
    - name: print the vaulted value
      ansible.builtin.debug:
        msg: "canary is {{ exocomp_canary }}"
`
	if err := os.WriteFile(playbook, []byte(body), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	return playbook
}
