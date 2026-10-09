package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// seedCutOff creates one job and moves it to a terminal cut-off state, the
// shape a forced restart or a crash leaves behind.
func seedCutOff(t *testing.T, st *store.Store, to store.JobState, runbook string, argv ...string) store.Job {
	t.Helper()
	if len(argv) == 0 {
		argv = []string{"/bin/true"}
	}
	job, err := st.CreateJob(context.Background(), store.NewJob{
		Runbook: runbook, Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: argv},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := st.TransitionJob(context.Background(), job.ID, store.StateQueued, to); err != nil {
		t.Fatalf("move job to %s: %v", to, err)
	}
	got, err := st.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return got
}

// seedSucceeded creates one job that ran to completion, which a redrive must
// never touch.
func seedSucceeded(t *testing.T, st *store.Store) store.Job {
	t.Helper()
	job, err := st.CreateJob(context.Background(), store.NewJob{
		Runbook: "done", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/true"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	ctx := context.Background()
	if err := st.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if err := st.TransitionJob(ctx, job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatalf("finish job: %v", err)
	}
	return job
}

// openSeededStore points at a scratch data dir and returns its store, so a
// test seeds rows and then drives `jobs redrive` through runRoot at the same
// directory.
func openSeededStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func countAllJobs(t *testing.T, st *store.Store) int {
	t.Helper()
	rows, err := st.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	return len(rows)
}

func stateOf(t *testing.T, st *store.Store, id int64) store.JobState {
	t.Helper()
	job, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("get job %d: %v", id, err)
	}
	return job.State
}

// TestJobsRedriveReenqueuesSelected: a redrive re-enqueues exactly the
// interrupted rows — each a fresh queued row carrying the same spec — and
// leaves the old rows and every other state alone.
func TestJobsRedriveReenqueuesSelected(t *testing.T) {
	st := openSeededStore(t)
	first := seedCutOff(t, st, store.StateInterrupted, "nightly", "/bin/true", "one")
	second := seedCutOff(t, st, store.StateInterrupted, "weekly", "/bin/true", "two")
	cancelled := seedCutOff(t, st, store.StateCancelled, "adhoc")
	done := seedSucceeded(t, st)

	out, _, err := runRoot(t, []string{"jobs", "redrive", "--yes"})
	if err != nil {
		t.Fatalf("jobs redrive --yes: %v (%s)", err, out)
	}
	for _, id := range []int64{first.ID, second.ID} {
		if !strings.Contains(out, "#"+strconv.FormatInt(id, 10)+" -> #") {
			t.Errorf("output %q does not map old #%d to its new row", out, id)
		}
	}
	if !strings.Contains(out, "redrove 2 job(s)") {
		t.Errorf("output %q does not summarise the two re-driven jobs", out)
	}

	if got := countAllJobs(t, st); got != 6 {
		t.Fatalf("store holds %d jobs, want 4 seeded + 2 re-driven", got)
	}
	for _, id := range []int64{first.ID, second.ID} {
		if got := stateOf(t, st, id); got != store.StateInterrupted {
			t.Errorf("old job %d is %s, want it left interrupted", id, got)
		}
	}
	if got := stateOf(t, st, cancelled.ID); got != store.StateCancelled {
		t.Errorf("cancelled job %d is %s, want it untouched by the default filter", cancelled.ID, got)
	}
	if got := stateOf(t, st, done.ID); got != store.StateSucceeded {
		t.Errorf("succeeded job %d is %s, want it untouched", done.ID, got)
	}

	rows, err := st.ListJobs(context.Background(), store.JobFilter{States: []store.JobState{store.StateQueued}})
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d queued rows, want the 2 re-driven ones", len(rows))
	}
	wantArgv := map[string]bool{"/bin/true one": false, "/bin/true two": false}
	for _, job := range rows {
		if job.Trigger != redriveTrigger {
			t.Errorf("new job %d trigger = %q, want %q", job.ID, job.Trigger, redriveTrigger)
		}
		key := strings.Join(job.Args.Argv, " ")
		if _, ok := wantArgv[key]; !ok {
			t.Errorf("new job %d carries unexpected argv %q", job.ID, key)
		} else {
			wantArgv[key] = true
		}
	}
	for key, seen := range wantArgv {
		if !seen {
			t.Errorf("no fresh queued row replays %q", key)
		}
	}
}

// TestJobsRedriveDryRun: --dry-run names the rows it would re-drive and
// enqueues nothing.
func TestJobsRedriveDryRun(t *testing.T) {
	st := openSeededStore(t)
	job := seedCutOff(t, st, store.StateInterrupted, "nightly")
	before := countAllJobs(t, st)

	out, _, err := runRoot(t, []string{"jobs", "redrive", "--dry-run"})
	if err != nil {
		t.Fatalf("jobs redrive --dry-run: %v (%s)", err, out)
	}
	if !strings.Contains(out, "would re-drive #"+strconv.FormatInt(job.ID, 10)) {
		t.Errorf("output %q does not name the interrupted job", out)
	}
	if !strings.Contains(out, "nothing enqueued") {
		t.Errorf("output %q does not say nothing was enqueued", out)
	}
	if got := countAllJobs(t, st); got != before {
		t.Fatalf("a dry run enqueued %d job(s)", got-before)
	}
}

// TestJobsRedriveStateFilter: --state selects which cut-off rows go. A
// cancelled-only run leaves interrupted rows alone, and vice versa.
func TestJobsRedriveStateFilter(t *testing.T) {
	st := openSeededStore(t)
	interrupted := seedCutOff(t, st, store.StateInterrupted, "nightly")
	cancelled := seedCutOff(t, st, store.StateCancelled, "adhoc")

	out, _, err := runRoot(t, []string{"jobs", "redrive", "--state", "cancelled", "--yes"})
	if err != nil {
		t.Fatalf("jobs redrive --state cancelled: %v (%s)", err, out)
	}
	if !strings.Contains(out, "#"+strconv.FormatInt(cancelled.ID, 10)+" -> #") {
		t.Errorf("output %q does not map the cancelled job", out)
	}
	if strings.Contains(out, "#"+strconv.FormatInt(interrupted.ID, 10)+" -> #") {
		t.Errorf("output %q re-drove an interrupted job outside the filter", out)
	}
	if got := countAllJobs(t, st); got != 3 {
		t.Fatalf("store holds %d jobs, want 2 seeded + 1 re-driven", got)
	}
}

// TestJobsRedriveDefaultIsInterrupted: with no --state only interrupted rows
// go, so a store holding just cancelled rows reports nothing to do.
func TestJobsRedriveDefaultIsInterrupted(t *testing.T) {
	st := openSeededStore(t)
	seedCutOff(t, st, store.StateCancelled, "adhoc")

	out, _, err := runRoot(t, []string{"jobs", "redrive", "--yes"})
	if err != nil {
		t.Fatalf("jobs redrive --yes: %v (%s)", err, out)
	}
	if !strings.Contains(out, "no jobs to redrive") {
		t.Errorf("output %q, want the empty-selection line", out)
	}
	if got := countAllJobs(t, st); got != 1 {
		t.Fatalf("store holds %d jobs, want the seeded row only", got)
	}
}

// TestJobsRedriveNeedsConfirmation: without --yes a non-interactive session is
// refused — never prompted, never hung — and --no-input refuses the same way.
// Nothing is enqueued either way.
func TestJobsRedriveNeedsConfirmation(t *testing.T) {
	st := openSeededStore(t)
	seedCutOff(t, st, store.StateInterrupted, "nightly")

	_, _, err := runRoot(t, []string{"jobs", "redrive"})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("bare redrive err = %v, want a refusal naming --yes", err)
	}
	_, _, err = runRoot(t, []string{"jobs", "redrive", "--no-input"})
	if err == nil || !strings.Contains(err.Error(), "non-interactive") {
		t.Fatalf("--no-input redrive err = %v, want the non-interactive refusal", err)
	}
	if got := countAllJobs(t, st); got != 1 {
		t.Fatalf("a refused redrive enqueued %d job(s)", got-1)
	}
}

// TestJobsRedriveJSON: --output json emits the redrive envelope carrying the
// old-to-new mapping, and the new row is really queued.
func TestJobsRedriveJSON(t *testing.T) {
	st := openSeededStore(t)
	job := seedCutOff(t, st, store.StateInterrupted, "nightly", "/bin/true", "one")

	out, _, err := runRoot(t, []string{"jobs", "redrive", "--yes", "--output", "json"})
	if err != nil {
		t.Fatalf("jobs redrive --output json: %v (%s)", err, out)
	}
	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Kind          string `json:"kind"`
		Data          struct {
			DryRun   bool `json:"dry_run"`
			Redriven []struct {
				From    int64  `json:"from"`
				To      int64  `json:"to"`
				Runbook string `json:"runbook"`
			} `json:"redriven"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("redrive output is not JSON: %v (%q)", err, out)
	}
	if env.SchemaVersion != wire.SchemaVersion || env.Kind != redriveKind {
		t.Fatalf("envelope = version %d kind %q, want %d %q",
			env.SchemaVersion, env.Kind, wire.SchemaVersion, redriveKind)
	}
	if env.Data.DryRun || len(env.Data.Redriven) != 1 {
		t.Fatalf("payload = %+v, want one real re-driven job", env.Data)
	}
	got := env.Data.Redriven[0]
	if got.From != job.ID || got.To == job.ID || got.To == 0 || got.Runbook != "nightly" {
		t.Fatalf("mapping = %+v, want from %d to a fresh row", got, job.ID)
	}
	if state := stateOf(t, st, got.To); state != store.StateQueued {
		t.Fatalf("new job %d is %s, want queued for the daemon to claim", got.To, state)
	}
}

// TestJobsRedriveJSONDryRun: a dry run in JSON mode reports the mapping with
// no new id and enqueues nothing.
func TestJobsRedriveJSONDryRun(t *testing.T) {
	st := openSeededStore(t)
	seedCutOff(t, st, store.StateInterrupted, "nightly")

	out, _, err := runRoot(t, []string{"jobs", "redrive", "--dry-run", "--output", "json"})
	if err != nil {
		t.Fatalf("dry-run json: %v (%s)", err, out)
	}
	var env struct {
		Kind string `json:"kind"`
		Data struct {
			DryRun   bool `json:"dry_run"`
			Redriven []struct {
				From int64 `json:"from"`
				To   int64 `json:"to"`
			} `json:"redriven"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("redrive output is not JSON: %v (%q)", err, out)
	}
	if env.Kind != redriveKind || !env.Data.DryRun || len(env.Data.Redriven) != 1 {
		t.Fatalf("payload kind %q = %+v, want one dry-run item", env.Kind, env.Data)
	}
	if env.Data.Redriven[0].To != 0 {
		t.Fatalf("dry-run item carries new id %d, want none", env.Data.Redriven[0].To)
	}
	if got := countAllJobs(t, st); got != 1 {
		t.Fatalf("a JSON dry run enqueued %d job(s)", got-1)
	}
}

// TestJobsRedriveLimit: --limit caps how many cut-off rows go.
func TestJobsRedriveLimit(t *testing.T) {
	st := openSeededStore(t)
	seedCutOff(t, st, store.StateInterrupted, "one")
	seedCutOff(t, st, store.StateInterrupted, "two")
	seedCutOff(t, st, store.StateInterrupted, "three")

	out, _, err := runRoot(t, []string{"jobs", "redrive", "--yes", "--limit", "1"})
	if err != nil {
		t.Fatalf("jobs redrive --limit 1: %v (%s)", err, out)
	}
	if !strings.Contains(out, "redrove 1 job(s)") {
		t.Errorf("output %q, want the one-job summary", out)
	}
	if got := countAllJobs(t, st); got != 4 {
		t.Fatalf("store holds %d jobs, want 3 seeded + 1 re-driven", got)
	}
}

// TestJobsRedriveRefusesLiveStates: only cut-off states go — asking for a
// live state is refused rather than duplicating work still in flight.
func TestJobsRedriveRefusesLiveStates(t *testing.T) {
	openSeededStore(t)

	_, _, err := runRoot(t, []string{"jobs", "redrive", "--state", "running", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "cannot be re-driven") {
		t.Fatalf("--state running err = %v, want the cannot-be-re-driven refusal", err)
	}
	_, _, err = runRoot(t, []string{"jobs", "redrive", "--state", "bogus", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "not a job state") {
		t.Fatalf("--state bogus err = %v, want the not-a-job-state refusal", err)
	}
}

// TestJobsRedriveLeavesLiveRowsAlone: a running row owned by a live process is
// neither swept nor re-driven — the sweep only retires rows a dead process
// left behind, and the filter only selects cut-off states.
func TestJobsRedriveLeavesLiveRowsAlone(t *testing.T) {
	st := openSeededStore(t)
	cutOff := seedCutOff(t, st, store.StateInterrupted, "nightly")
	live, err := st.CreateJob(context.Background(), store.NewJob{
		Runbook: "live", Kind: "shell", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"/bin/sleep", "30"}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := st.ClaimJob(context.Background(), live.ID); err != nil {
		t.Fatalf("claim job: %v", err)
	}

	out, _, err := runRoot(t, []string{"jobs", "redrive", "--yes"})
	if err != nil {
		t.Fatalf("jobs redrive --yes: %v (%s)", err, out)
	}
	if !strings.Contains(out, "redrove 1 job(s)") {
		t.Errorf("output %q, want exactly the cut-off job re-driven", out)
	}
	if got := stateOf(t, st, live.ID); got != store.StateRunning {
		t.Errorf("live job %d is %s, want it left running", live.ID, got)
	}
	if got := stateOf(t, st, cutOff.ID); got != store.StateInterrupted {
		t.Errorf("cut-off job %d is %s, want it left interrupted", cutOff.ID, got)
	}
	if got := countAllJobs(t, st); got != 3 {
		t.Fatalf("store holds %d jobs, want 2 seeded + 1 re-driven", got)
	}
}

// TestPromptRedriveAnswers: the TTY prompt proceeds on an explicit yes and
// aborts on anything else, enqueueing nothing either way. The prompt reader is
// exercised directly: stdinIsInteractive needs a real terminal, which a test
// never has.
func TestPromptRedriveAnswers(t *testing.T) {
	newCmd := func(input string) *cobra.Command {
		cmd := newJobsRedriveCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetIn(strings.NewReader(input))
		return cmd
	}
	if err := promptRedrive(newCmd("yes\n"), 2); err != nil {
		t.Fatalf("prompt with yes: %v", err)
	}
	if err := promptRedrive(newCmd("y\n"), 2); err != nil {
		t.Fatalf("prompt with y: %v", err)
	}
	for _, input := range []string{"\n", "no\n", "yessir\n"} {
		if err := promptRedrive(newCmd(input), 2); err == nil {
			t.Fatalf("prompt with %q proceeded, want an abort", input)
		}
	}
}

// TestRedriveSpecReplaysTheRow: the rebuilt spec carries the row's run facts
// and drops the keys that would collapse the enqueue onto the original row.
func TestRedriveSpecReplaysTheRow(t *testing.T) {
	job := store.Job{
		ID: 41, Runbook: "nightly", Kind: "shell", Trigger: "schedule",
		ConcurrencyKey: "backup:prod", DedupKey: "check:web:up", IdempotencyKey: "sched:9",
		Args: store.JobArgs{
			Argv: []string{"/bin/true", "one"}, Dir: "/tmp/tree", DirReadOnly: true,
			VaultProject: "fleet", RunSecretBinding: "run", Inventory: "home",
			Source: "infra", ExtraVars: map[string]string{"env": "prod"},
			RunnerName: "big", TolerateUnreachable: true, Timeout: store.Duration(0),
			Privileged: true, EgressPorts: []int{2222}, SSHIdentityFiles: []string{"/keys/fleet"},
		},
	}
	spec := redriveSpec(job)
	if spec.Trigger != redriveTrigger {
		t.Errorf("trigger = %q, want %q", spec.Trigger, redriveTrigger)
	}
	if strings.Join(spec.Argv, " ") != "/bin/true one" || spec.Runbook != "nightly" ||
		spec.Kind != "shell" || spec.ConcurrencyKey != "backup:prod" ||
		spec.Source != "infra" || spec.Inventory != "home" || !spec.Privileged {
		t.Errorf("spec drops a run fact: %+v", spec)
	}
	if spec.IdempotencyKey != "" || spec.DedupKey != "" {
		t.Errorf("spec carries dedup keys %+v, which would collapse the enqueue", spec)
	}
	// The clone must not alias the row's argv.
	spec.Argv[0] = "/bin/false"
	if job.Args.Argv[0] != "/bin/true" {
		t.Error("the rebuilt argv aliases the row's argv")
	}
}
