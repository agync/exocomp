package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestJobArgsEgressPortsRoundTrip: an ansible job's declared egress ports ride
// on the job row as egress_ports, surviving a marshal/unmarshal so a daemon
// that claims the row rebuilds the same grant. Empty stays omitted, so an
// ordinary job row reads exactly as before.
func TestJobArgsEgressPortsRoundTrip(t *testing.T) {
	t.Parallel()

	in := store.JobArgs{Argv: []string{"ansible-playbook", "site.yml"}, EgressPorts: []int{2222, 22}}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !slices.Contains(bytesFields(t, body), "egress_ports") {
		t.Fatalf("egress_ports missing from %s", body)
	}
	var out store.JobArgs
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !slices.Equal(out.EgressPorts, []int{2222, 22}) {
		t.Fatalf("round-trip EgressPorts = %v, want [2222 22]", out.EgressPorts)
	}

	plain, err := json.Marshal(store.JobArgs{Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	if slices.Contains(bytesFields(t, plain), "egress_ports") {
		t.Fatalf("an empty egress set must stay omitted: %s", plain)
	}
}

// TestJobEgressPortsSurviveTheStore: the row the queue writes and a daemon
// reads back carries the declared egress ports, the same path a re-run clones.
func TestJobEgressPortsSurviveTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := open(t)

	job, err := s.CreateJob(ctx, store.NewJob{
		Runbook: "playbook", Kind: "ansible", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"ansible-playbook", "site.yml"}, EgressPorts: []int{2222}},
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	got, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !slices.Equal(got.Args.EgressPorts, []int{2222}) {
		t.Fatalf("stored EgressPorts = %v, want [2222]", got.Args.EgressPorts)
	}
}

// TestRuleActionEgressRoundTrip: a playbook-action rule's declared egress
// rides in the action as egress, so the reactive builder reads the same grant
// back when the rule fires. Empty stays omitted.
func TestRuleActionEgressRoundTrip(t *testing.T) {
	t.Parallel()

	in := store.RuleAction{Source: "ops", Playbook: "site.yml", Limit: "laptop", Egress: []int{2222}}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !slices.Contains(bytesFields(t, body), "egress") {
		t.Fatalf("egress missing from %s", body)
	}
	var out store.RuleAction
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !slices.Equal(out.Egress, []int{2222}) {
		t.Fatalf("round-trip Egress = %v, want [2222]", out.Egress)
	}

	plain, err := json.Marshal(store.RuleAction{Source: "ops"})
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	if slices.Contains(bytesFields(t, plain), "egress") {
		t.Fatalf("an empty egress set must stay omitted: %s", plain)
	}
}

// TestRuleEgressSurvivesTheStore: the rule row the engine loads carries the
// declared egress ports when a match fires.
func TestRuleEgressSurvivesTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := open(t)

	created, err := s.CreateRule(ctx, store.NewRule{
		Name: "r", CELExpr: "true", ActionKind: store.ActionPlaybook,
		Action: store.RuleAction{Source: "ops", Playbook: "site.yml", Limit: "laptop", Egress: []int{2222}},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	got, err := s.GetRule(ctx, created.Name)
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if !slices.Equal(got.Action.Egress, []int{2222}) {
		t.Fatalf("stored Egress = %v, want [2222]", got.Action.Egress)
	}
}

// TestNormalizeEgressPorts: the exported seam every write face validates
// through sorts, de-duplicates, and refuses a port outside 1..65535 — a junk
// declaration is an operator mistake worth surfacing, not a posture to quietly
// widen or narrow. Widening the range check in normalizeEgressPorts fails this
// test.
func TestNormalizeEgressPorts(t *testing.T) {
	t.Parallel()

	clean, err := store.NormalizeEgressPorts([]int{443, 2222, 22, 2222})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if want := []int{22, 443, 2222}; !slices.Equal(clean, want) {
		t.Fatalf("NormalizeEgressPorts = %v, want %v (sorted, deduped)", clean, want)
	}

	for _, bad := range []int{0, -1, 65536, 70000} {
		if _, err := store.NormalizeEgressPorts([]int{bad}); !errors.Is(err, store.ErrInvalidEgressPort) {
			t.Errorf("NormalizeEgressPorts(%d) err = %v, want ErrInvalidEgressPort", bad, err)
		}
	}
	if _, err := store.NormalizeEgressPorts([]int{22, 70000}); !errors.Is(err, store.ErrInvalidEgressPort) {
		t.Errorf("mixed set err = %v, want ErrInvalidEgressPort (no partial accept)", err)
	}
}

// bytesFields is a tiny helper for JSON field-name assertions: it splits a JSON
// body on the shell-unsafe token separators into field-ish tokens.
func bytesFields(t *testing.T, body []byte) []string {
	t.Helper()
	var tokens []string
	start := -1
	for i, b := range body {
		if b == '"' {
			if start < 0 {
				start = i
			} else {
				tokens = append(tokens, string(body[start+1:i]))
				start = -1
			}
		}
	}
	return tokens
}
