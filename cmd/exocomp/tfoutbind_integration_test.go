package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// tfBindSentinel is the value the stand-in tofu's sensitive output carries. It
// may reach the play's environment and nothing else — not an argv, not an
// event, not the ledger.
const tfBindSentinel = "TFOUT-BIND-SENTINEL-4a91c7"

// tfBindOutputs is the `tofu output -json` the stand-in prints: a host-shaped
// inventory, a sensitive output whose name folds to a valid env identifier, and
// a sensitive output whose name cannot.
const tfBindOutputs = `{"ansible_inventory":{"sensitive":false,"value":` +
	`{"all":{"hosts":{"localhost":{"ansible_connection":"local"}}}}},` +
	`"db_password":{"sensitive":true,"value":"` + tfBindSentinel + `"},` +
	`"db-password":{"sensitive":true,"value":"` + tfBindSentinel + `-dashed"}}`

// TestWorkspaceApplyBindsOutputIntoDependentPlay is the item-D loop at the
// operator's level: an apply seals its sensitive outputs and binds them, a
// re-apply does not duplicate the binding, a play against the workspace's
// standing inventory receives the value as $DB_PASSWORD with no flag, a play
// that does not target it receives nothing, and an output whose name cannot be
// an env identifier is refused a binding with a reason the operator can read.
func TestWorkspaceApplyBindsOutputIntoDependentPlay(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)
	// Stand-in tofu: init/apply succeed, `output -json` prints the outputs. The
	// same loop runs against real tofu in internal/executor's bind gate.
	fakeTool(t, executor.TofuBinary,
		"case \"$1\" in output) printf '%s' '"+tfBindOutputs+"' ;; esac\nexit 0")
	// Stand-in ansible-playbook: report the bound secret by LENGTH, never by
	// value, which is the only report a test of a secret may print.
	fakeTool(t, executor.AnsibleBinary, `echo "DBPWLEN=${#DB_PASSWORD}"
echo "DASHED=${#DB_PASSWORD_DASHED}"
exit 0`)

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	// No daemon: this CLI runs the apply inline, captures, seals, and binds.
	out, code := runCLIExit(t, bin, "workspace", "apply", "wsp")
	if code != 0 {
		t.Fatalf("workspace apply exited %d:\n%s", code, out)
	}
	// The refusal is operator-visible and says why, and the name was not
	// rewritten into something the module never declared.
	if !strings.Contains(out, "db-password: not bound") ||
		!strings.Contains(out, "POSIX identifier") {
		t.Errorf("apply did not report the unbindable output:\n%s", out)
	}
	if strings.Contains(out, tfBindSentinel) {
		t.Fatalf("the apply's own output carried a sealed value:\n%s", out)
	}

	// The binding is there, under the workspace's standing name, mapping the
	// upper-cased output to its deterministic secret row.
	listing, code := runCLIExit(t, bin, "run-secret", "list", "tf:wsp")
	if code != 0 {
		t.Fatalf("run-secret list exited %d:\n%s", code, listing)
	}
	if !strings.Contains(listing, "DB_PASSWORD") ||
		!strings.Contains(listing, "tf-output/wsp/db_password") {
		t.Fatalf("binding tf:wsp does not map the sealed output:\n%s", listing)
	}
	if strings.Contains(listing, "DB-PASSWORD") || strings.Contains(listing, "DB_PASSWORD_DASHED") {
		t.Errorf("the unbindable output was mangled into an env name:\n%s", listing)
	}

	// A re-apply updates the binding in place.
	if out, code := runCLIExit(t, bin, "workspace", "apply", "wsp"); code != 0 {
		t.Fatalf("re-apply exited %d:\n%s", code, out)
	}
	again, _ := runCLIExit(t, bin, "run-secret", "list", "tf:wsp")
	if got := strings.Count(again, "DB_PASSWORD"); got != 1 {
		t.Fatalf("re-apply left %d DB_PASSWORD pair(s), want 1:\n%s", got, again)
	}

	// The dependent play names the workspace's standing inventory and nothing
	// else, and receives the sealed value as named env.
	play, code := runCLIExit(t, bin, "playbook", "run", playbook, "--inventory", "tf:wsp")
	if code != 0 {
		t.Fatalf("playbook run exited %d:\n%s", code, play)
	}
	if want := "DBPWLEN=" + strconv.Itoa(len(tfBindSentinel)); !strings.Contains(play, want) {
		t.Errorf("the play did not receive $DB_PASSWORD (%s):\n%s", want, play)
	}
	if !strings.Contains(play, "DASHED=0") {
		t.Errorf("the unbindable output reached the play under some env name:\n%s", play)
	}
	assertNoSentinel(t, play, "playbook run")

	// An unrelated run — same play, no workspace inventory — inherits nothing.
	other, code := runCLIExit(t, bin, "playbook", "run", playbook)
	if code != 0 {
		t.Fatalf("unrelated playbook run exited %d:\n%s", code, other)
	}
	if !strings.Contains(other, "DBPWLEN=0") {
		t.Errorf("a run outside the workspace's chain inherited the sealed output:\n%s", other)
	}

	// The durable ledger and the recorded argv carry the binding, never a value.
	assertPlayLedgerValuesFree(t, play)
}

// assertNoSentinel fails if a rendered surface carries the sealed value.
func assertNoSentinel(t *testing.T, text, what string) {
	t.Helper()
	if strings.Contains(text, tfBindSentinel) {
		t.Errorf("%s carried the sealed value:\n%s", what, text)
	}
}

// assertPlayLedgerValuesFree re-reads the play's job straight from the store and
// proves the value is in neither its recorded argv nor any of its events.
func assertPlayLedgerValuesFree(t *testing.T, cliOutput string) {
	t.Helper()
	m := regexp.MustCompile(`job (\d+)`).FindStringSubmatch(cliOutput)
	if m == nil {
		t.Fatalf("no job id in the play's output:\n%s", cliOutput)
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("parse job id %q: %v", m[1], err)
	}
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	job, err := st.GetJob(t.Context(), id)
	if err != nil {
		t.Fatalf("get job %d: %v", id, err)
	}
	assertNoSentinel(t, strings.Join(job.Args.Argv, " "), "the job's argv")
	assertNoSentinel(t, jobEventText(t, id), "the job's events")
}
