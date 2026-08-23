package wire_test

import (
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// TestGoldenPlanOutcomeChanges pins the bytes for a plan job that found
// changes: JobView.Plan carries changes:true, add/destroy counts (a replace
// counts in both), and the resource addresses — never attribute values.
func TestGoldenPlanOutcomeChanges(t *testing.T) {
	job := store.Job{
		ID: 42, Runbook: "workspace:web", Kind: "tofu", Trigger: "manual",
		State: store.JobState("succeeded"), ExitCode: ptrInt(0),
		QueuedAt: at("2026-01-02T03:04:05Z"),
	}
	events := []store.JobEvent{
		{Seq: 1, Ts: at("2026-01-02T03:04:06Z"), Level: store.LevelInfo, Msg: "tofu plan starting"},
		{Seq: 2, Ts: at("2026-01-02T03:04:09Z"), Level: store.LevelSystem, Msg: "tofu plan: changes present", RawJSON: `{"tofu":{"operation":"plan","changes":true,"pending":[` +
			`{"address":"aws_instance.web","actions":["create"]},` +
			`{"address":"aws_db_instance.main","actions":["delete","create"]}` +
			`]}}`},
	}

	view := wire.NewJobView(job)
	view.Plan = wire.NewPlanOutcomeView(events)

	got, err := wire.MarshalSuccess(wire.KindJob, view)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"job.get","data":{"id":42,"runbook":"workspace:web","kind":"tofu",` +
		`"trigger":"manual","state":"succeeded","exit_code":0,"queued_at":"2026-01-02T03:04:05Z",` +
		`"plan":{"changes":true,"add":2,"destroy":1,"addresses":["aws_instance.web","aws_db_instance.main"]}}}` + "\n"
	if string(got) != want {
		t.Fatalf("plan-outcome bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestGoldenPlanOutcomeNoChanges pins the no-change case: Plan is present but
// changes is false, with every count and Addresses omitted.
func TestGoldenPlanOutcomeNoChanges(t *testing.T) {
	events := []store.JobEvent{
		{Seq: 1, Ts: at("2026-01-02T03:04:09Z"), Level: store.LevelSystem, Msg: "tofu plan: no changes",
			RawJSON: `{"tofu":{"operation":"plan","changes":false}}`},
	}
	view := wire.JobView{ID: 1, State: "succeeded"}
	view.Plan = wire.NewPlanOutcomeView(events)

	got, err := wire.MarshalSuccess(wire.KindJob, view)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"job.get","data":{"id":1,"state":"succeeded","plan":{"changes":false}}}` + "\n"
	if string(got) != want {
		t.Fatalf("no-change plan bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestPlanOutcomeAbsentForNonPlanJob asserts a job whose events carry no
// tofu/ansible_check payload (an ordinary shell run) projects no plan field
// at all, not a false one — nil means "not a plan-family job", false means
// "a plan ran and found nothing".
func TestPlanOutcomeAbsentForNonPlanJob(t *testing.T) {
	events := []store.JobEvent{
		{Seq: 1, Ts: at("2026-01-02T03:04:06Z"), Level: store.LevelInfo, Msg: "running /bin/true"},
	}
	if v := wire.NewPlanOutcomeView(events); v != nil {
		t.Fatalf("NewPlanOutcomeView on a non-plan job = %+v, want nil", v)
	}

	view := wire.JobView{ID: 9, State: "succeeded"}
	view.Plan = wire.NewPlanOutcomeView(events)
	got, err := wire.MarshalSuccess(wire.KindJob, view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), `"plan"`) {
		t.Fatalf("non-plan job must carry no plan field: %s", got)
	}
}

// TestPlanOutcomeNoAttributeValues seeds raw_json carrying a fake secret
// value on fields the outcome structs (TofuPlanOutcome/ResourceDelta,
// AnsibleCheckOutcome/AnsibleHostDrift) do not declare, so unmarshalling into
// them structurally drops it. This is the regression guard: even if a future
// driver bug added a value alongside the real fields, PlanOutcomeView has no
// field for it to land in.
func TestPlanOutcomeNoAttributeValues(t *testing.T) {
	const secret = "sk-FAKE-SECRET-VALUE-MUST-NOT-LEAK"

	tofuEvents := []store.JobEvent{{
		Seq: 1, Level: store.LevelSystem,
		RawJSON: `{"tofu":{"operation":"plan","changes":true,"pending":[` +
			`{"address":"aws_db_instance.main","actions":["update"],"attr_keys":["password"],"password":"` + secret + `"}` +
			`]}}`,
	}}
	tofuGot, err := wire.MarshalSuccess(wire.KindJob, wire.JobView{ID: 1, State: "succeeded", Plan: wire.NewPlanOutcomeView(tofuEvents)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tofuGot), secret) {
		t.Fatalf("tofu plan outcome leaked an attribute value: %s", tofuGot)
	}

	ansibleEvents := []store.JobEvent{{
		Seq: 1, Level: store.LevelSystem,
		RawJSON: `{"ansible_check":{"operation":"check","changes":true,"drift":[` +
			`{"host":"node1","changed":2,"diff":"password_hash: ` + secret + `"}` +
			`]}}`,
	}}
	ansibleGot, err := wire.MarshalSuccess(wire.KindJob, wire.JobView{ID: 2, State: "succeeded", Plan: wire.NewPlanOutcomeView(ansibleEvents)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ansibleGot), secret) {
		t.Fatalf("ansible check outcome leaked an attribute value: %s", ansibleGot)
	}
}

// TestGoldenPlanOutcomeAnsibleCheck pins the ansible check-mode twin: a
// drifted host counts as one Change (ansible has no add/destroy split) and is
// named by host, not resource address.
func TestGoldenPlanOutcomeAnsibleCheck(t *testing.T) {
	events := []store.JobEvent{{
		Seq: 1, Level: store.LevelSystem,
		RawJSON: `{"ansible_check":{"operation":"check","changes":true,"clean_hosts":1,"drift":[` +
			`{"host":"node1","changed":3}` +
			`]}}`,
	}}
	view := wire.JobView{ID: 5, State: "succeeded"}
	view.Plan = wire.NewPlanOutcomeView(events)
	got, err := wire.MarshalSuccess(wire.KindJob, view)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"job.get","data":{"id":5,"state":"succeeded",` +
		`"plan":{"changes":true,"change":1,"addresses":["node1"]}}}` + "\n"
	if string(got) != want {
		t.Fatalf("ansible check-outcome bytes drift:\n got: %s\nwant: %s", got, want)
	}
}

// TestJobEventsCarryPlanOnTerminalEventOnly asserts NewJobEventList attaches
// Plan only to the one event whose raw_json is the outcome payload, not to
// every event in the job.
func TestJobEventsCarryPlanOnTerminalEventOnly(t *testing.T) {
	events := []store.JobEvent{
		{Seq: 1, Ts: at("2026-01-02T03:04:06Z"), Level: store.LevelInfo, Msg: "tofu plan starting"},
		{Seq: 2, Ts: at("2026-01-02T03:04:09Z"), Level: store.LevelSystem, Msg: "tofu plan: changes present",
			RawJSON: `{"tofu":{"operation":"plan","changes":true}}`},
	}
	views := wire.NewJobEventList(events)
	if views[0].Plan != nil {
		t.Fatalf("event 1 (no outcome payload) got Plan = %+v, want nil", views[0].Plan)
	}
	if views[1].Plan == nil || !views[1].Plan.Changes {
		t.Fatalf("event 2 (the outcome payload) got Plan = %+v, want {Changes:true}", views[1].Plan)
	}
}
