package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// remoteSource registers a path source with a default egress port on the
// daemon, the configuration a remote ad-hoc run must pick up there.
func remoteSource(t *testing.T, d *testDaemon) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ansible.cfg"), []byte("[defaults]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := d.st.CreateSource(ctx, store.Source{Name: "infra", Kind: store.SourcePath, URI: root, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.SetSourceEgressPorts(ctx, src.ID, []int{443}); err != nil {
		t.Fatal(err)
	}
	return root
}

func onlyJob(t *testing.T, d *testDaemon) store.Job {
	t.Helper()
	jobs, err := d.st.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("daemon has %d jobs, want 1", len(jobs))
	}
	return jobs[0]
}

// `command --source` with a remote selected runs on the daemon, from the
// source's checkout there, with the source's egress default applied, and the
// CLI follows it to the end.
func TestRemoteCommandRunsOnTheDaemon(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	root := remoteSource(t, d)

	out, _, err := runRoot(t, []string{"--remote", "box", "command", "--source", "infra",
		"--run-secrets", "api", "--host", "web", "--", "echo", "hello"})
	if err != nil {
		t.Fatalf("remote command: %v\n%s", err, out)
	}
	if !strings.Contains(out, "queued for the remote daemon") || !strings.Contains(out, "hello") ||
		!strings.Contains(out, "state: succeeded") {
		t.Fatalf("output does not follow the remote job:\n%s", out)
	}

	job := onlyJob(t, d)
	if job.Runbook != "command" || job.Args.Source != "infra" || job.Args.Dir != root {
		t.Errorf("job = %q source %q dir %q, want command from infra at %q",
			job.Runbook, job.Args.Source, job.Args.Dir, root)
	}
	if !slices.Contains(job.Args.EgressPorts, 443) {
		t.Errorf("egress = %v, want the source default 443", job.Args.EgressPorts)
	}
	if job.Args.RunSecretBinding != "api" {
		t.Errorf("run-secrets = %q, want api", job.Args.RunSecretBinding)
	}
	ran := d.specs()
	if len(ran) != 1 || ran[0].Dir != root || !strings.Contains(strings.Join(ran[0].Argv, " "), "echo hello") {
		t.Fatalf("worker ran %+v, want echo hello from %s", ran, root)
	}
	assertNoLocalStore(t)
}

// Under --output json a remote `ansible` prints the same command.result
// document a local run does.
func TestRemoteAnsibleJSONIsTheCommandResult(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)

	out, _, err := runRoot(t, []string{"--remote", "box", "--output", "json", "ansible", "--host", "web", "-m", "ping"})
	if err != nil {
		t.Fatalf("remote ansible: %v\n%s", err, out)
	}
	var env struct {
		Kind string                 `json:"kind"`
		Data wire.CommandResultView `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out)
	}
	job := onlyJob(t, d)
	if env.Kind != wire.KindCommandResult || env.Data.JobID != job.ID || env.Data.State != "succeeded" {
		t.Fatalf("envelope = %+v, want command.result for job %d", env, job.ID)
	}
	if _, ok := env.Data.Hosts["node0"]; !ok {
		t.Errorf("hosts = %+v, want node0 parsed from the job's events", env.Data.Hosts)
	}
	if job.Runbook != "ansible" {
		t.Errorf("runbook = %q, want ansible", job.Runbook)
	}
	assertNoLocalStore(t)
}

func TestRemoteCommandRequireApprovalWaitsOnTheDaemon(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)

	out, _, err := runRoot(t, []string{"--remote", "box", "command", "--require-approval", "--host", "web", "--", "reboot"})
	if err != nil {
		t.Fatalf("gated remote command: %v\n%s", err, out)
	}
	job := onlyJob(t, d)
	if job.State != store.StateAwaitingApproval {
		t.Fatalf("job state = %s, want awaiting_approval", job.State)
	}
	if !strings.Contains(out, "gated") || !strings.Contains(out, "--remote box approval decide") {
		t.Fatalf("output = %q, want the gated job and the decide command", out)
	}
	if len(d.specs()) != 0 {
		t.Fatal("a gated job ran")
	}
}

func TestRemoteAdhocRefusesInputsThatNameThisMachine(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)

	cases := map[string]struct {
		args []string
		want string
	}{
		"privileged":        {[]string{"command", "--privileged", "--host", "web", "--", "id"}, "applies to a local run"},
		"inventory path":    {[]string{"command", "--inventory", "./hosts.ini", "--host", "web", "--", "id"}, "is a path on this machine"},
		"relative identity": {[]string{"ansible", "--ssh-identity", "id_ed25519", "--host", "web", "-m", "ping"}, "is not absolute"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := runRoot(t, append([]string{"--remote", "box"}, tc.args...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal saying %q, got %v", tc.want, err)
			}
		})
	}
	jobs, err := d.st.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a refused run enqueued %d job(s)", len(jobs))
	}
	assertNoLocalStore(t)
}

func TestRemoteCommandNeedsAnOperatorToken(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeReadonly, true)

	_, _, err := runRoot(t, []string{"--remote", "box", "command", "--host", "web", "--", "id"})
	if err == nil || !strings.Contains(err.Error(), "operator") {
		t.Fatalf("want a forbidden error naming the operator scope, got %v", err)
	}
	if jobs, _ := d.st.ListJobs(context.Background(), store.JobFilter{}); len(jobs) != 0 {
		t.Fatalf("a refused run enqueued %d job(s)", len(jobs))
	}
}
