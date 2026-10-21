package main

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/hooks"
)

// TestParseSSHIdentityFiles: --ssh-identity values parse to an absolutized,
// de-duplicated, sorted path set. A relative declaration is resolved against
// the caller's working directory, exactly like the playbook path.
func TestParseSSHIdentityFiles(t *testing.T) {
	t.Parallel()

	abs := filepath.Join(t.TempDir(), "id_ed25519")
	got, err := parseSSHIdentityFiles([]string{abs, "keys/lima", abs})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rel, err := filepath.Abs("keys/lima")
	if err != nil {
		t.Fatalf("abs resolve: %v", err)
	}
	want := []string{rel, abs}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("parseSSHIdentityFiles = %v, want %v (absolutized, deduped, sorted)", got, want)
	}

	empty, err := parseSSHIdentityFiles(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("no declaration = %v (%v), want empty", empty, err)
	}
}

// TestComposeRunStepsCarriesSSHIdentity: `run --ssh-identity` carries its
// declared files onto the play step of the composed workflow, the same way the
// other run options do — a hook/gate composed run must not silently drop the
// grant.
func TestComposeRunStepsCarriesSSHIdentity(t *testing.T) {
	hk := hooks.Hooks{PreRun: []hooks.Hook{{Name: "prep", Run: "true"}}}
	steps, err := composeRunSteps("/src", false, "/src/site.yml", "", "",
		runSpec{sourceName: "infra", sshIdentityFiles: []string{"/keys/lima"}}, hk)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("want pre-hook + play steps, got %d", len(steps))
	}
	if !slices.Equal(steps[1].Spec.SSHIdentityFiles, []string{"/keys/lima"}) {
		t.Fatalf("play step SSHIdentityFiles = %v, want [/keys/lima]", steps[1].Spec.SSHIdentityFiles)
	}
}

// TestPlaybookRunSSHIdentityFilesReachJobRow is the plain local path's
// no-silent-drop proof: a playbook run declared with identity files lands them
// on the job row, where a daemon that claims the run grants them to the
// sandbox. The run itself is unsandboxed (the disabled tier the CLI tests
// pin), so the assertion is on the stored grant, not on loading a key.
func TestPlaybookRunSSHIdentityFilesReachJobRow(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible-playbook")

	dir := t.TempDir()
	play := filepath.Join(dir, "site.yml")
	writeFile(t, play,
		"- hosts: localhost\n  gather_facts: false\n  tasks:\n"+
			"    - name: noop\n      ansible.builtin.command: /bin/true\n      changed_when: false\n")
	inventory := filepath.Join(dir, "hosts.ini")
	writeFile(t, inventory, "localhost ansible_connection=local\n")

	identity := filepath.Join(t.TempDir(), "id_ed25519")
	var out, warn bytes.Buffer
	code, err := runPlaybook(context.Background(), &out, &warn, playbookSpec{
		playbook:         play,
		inventory:        inventory,
		sshIdentityFiles: []string{identity},
	})
	if err != nil || code != 0 {
		t.Fatalf("playbook run: code=%d err=%v\n%s", code, err, out.String())
	}

	id := lastJobID(t, "playbook")
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	job, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(job.Args.SSHIdentityFiles, []string{identity}) {
		t.Fatalf("job SSHIdentityFiles = %v, want [%s]", job.Args.SSHIdentityFiles, identity)
	}
}

// TestPlaybookRunRemoteRelativeSSHIdentityRefused: a remote run's identity is
// a path on the daemon, so a relative one is refused before any request,
// rather than absolutized against this machine's working directory.
func TestPlaybookRunRemoteRelativeSSHIdentityRefused(t *testing.T) {
	pinnedRemote(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a relative remote identity must not reach the daemon")
	}))

	_, _, err := runRoot(t, []string{"playbook", "run", "site.yml", "--source", "demo",
		"--remote", "box", "--ssh-identity", "keys/lima"})
	if err == nil {
		t.Fatalf("a remote run accepted a relative --ssh-identity")
	}
	if !strings.Contains(err.Error(), "ssh-identity") {
		t.Errorf("remote refusal = %v, want it to name --ssh-identity", err)
	}
}

// seedSSHIdentityRuleSource stands up the data dir + source + playbook a
// playbook rule-add needs, for the identity assertions below.
func seedSSHIdentityRuleSource(t *testing.T) {
	t.Helper()
	seedPlaybookSource(t, "")
}

// TestRuleAddPlaybookSSHIdentityThreadsToRule: `rule add ... --action playbook
// --ssh-identity /keys/lima` stores the declared files on the rule action, so
// a fired rule runs its play with the same grant a CLI run gets.
func TestRuleAddPlaybookSSHIdentityThreadsToRule(t *testing.T) {
	seedSSHIdentityRuleSource(t)

	identity := filepath.Join(t.TempDir(), "id_ed25519")
	out, err := run(t, "rule", "add", "laptop-update",
		"--source", "generic",
		"--expr", `event.name == "home" && event.status == "firing"`,
		"--action", "playbook",
		"--repo-source", "ops",
		"--playbook", "plays/site.yml",
		"--limit", "laptop",
		"--ssh-identity", identity,
	)
	if err != nil {
		t.Fatalf("rule add playbook --ssh-identity: %v (%s)", err, out)
	}
	if !strings.Contains(out, "rule laptop-update (playbook) added") {
		t.Fatalf("add output = %q", out)
	}

	// The stored rule carries the declared identity on its action, so the
	// reactive builder reads it back when the rule fires.
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	rule, err := st.GetRule(context.Background(), "laptop-update")
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if !slices.Equal(rule.Action.SSHIdentityFiles, []string{identity}) {
		t.Fatalf("stored rule SSHIdentityFiles = %v, want [%s]", rule.Action.SSHIdentityFiles, identity)
	}
}

// TestAdhocSSHIdentityReachesJobRow is the ad-hoc plumbing proof: both
// `exocomp command` and `exocomp ansible` accept --ssh-identity and land the
// declared files on the job row, the same seam `playbook run` uses, so a
// one-off against a key-only host does not have to reach for --privileged.
// Dropping the parse or the queued-spec assignment in either verb leaves the
// row empty and fails here.
func TestAdhocSSHIdentityReachesJobRow(t *testing.T) {
	useDataDir(t, t.TempDir())
	requireTools(t, "ansible")

	for _, tc := range []struct {
		name    string
		runbook string
		args    func(identity string) []string
	}{
		{"command", "command", func(id string) []string {
			return []string{"command", "--host", "localhost", "--ssh-identity", id, "--", "/bin/true"}
		}},
		{"ansible", "ansible", func(id string) []string {
			return []string{"ansible", "--host", "localhost", "-m", "ansible.builtin.ping", "--ssh-identity", id}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := filepath.Join(t.TempDir(), "id_ed25519")
			out, err := run(t, tc.args(identity)...)
			if err != nil {
				t.Fatalf("%s --ssh-identity: %v (%s)", tc.runbook, err, out)
			}
			job := getJob(t, lastJobID(t, tc.runbook))
			if !slices.Equal(job.Args.SSHIdentityFiles, []string{identity}) {
				t.Fatalf("job SSHIdentityFiles = %v, want [%s] (%s)",
					job.Args.SSHIdentityFiles, identity, out)
			}
		})
	}
}
