package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestRuleActionLimitHostsRoundTrip: the governed host-set expansion flag rides
// action_args_json, survives a create/get, and is omitted from the encoding
// when off so a fixed-limit rule's JSON is unchanged.
func TestRuleActionLimitHostsRoundTrip(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateRule(ctx, store.NewRule{
		Name: "present-update", CELExpr: "true", ActionKind: store.ActionPlaybook,
		Action: store.RuleAction{
			Source: "ops", Playbook: "plays/site.yml", LimitHosts: true,
		},
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if !created.Action.LimitHosts || created.Action.Limit != "" {
		t.Fatalf("created action = %+v, want limit_hosts true and no fixed limit", created.Action)
	}
	got, err := st.GetRule(ctx, "present-update")
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if !got.Action.LimitHosts || got.Action.Limit != "" {
		t.Fatalf("action = %+v, want limit_hosts true and no fixed limit", got.Action)
	}

	fixed, err := st.CreateRule(ctx, store.NewRule{
		Name: "fixed", CELExpr: "true", ActionKind: store.ActionPlaybook,
		Action: store.RuleAction{Source: "ops", Playbook: "plays/site.yml", Limit: "laptop"},
	})
	if err != nil {
		t.Fatalf("create fixed rule: %v", err)
	}
	blob, err := json.Marshal(fixed.Action)
	if err != nil {
		t.Fatalf("marshal action: %v", err)
	}
	if strings.Contains(string(blob), "limit_hosts") {
		t.Fatalf("a fixed-limit action encoded limit_hosts: %s", blob)
	}
}

// TestSetCheckAggregate: the aggregate config setter flips the flag, bounds the
// threshold against the current target count, and reports a missing check.
func TestSetCheckAggregate(t *testing.T) {
	t.Parallel()
	st := openReactive(t)
	ctx := context.Background()

	created, err := st.CreateCheck(ctx, store.NewCheck{
		Name: "home", Kind: store.CheckTCP, IntervalS: 300, Targets: checkFixture().Targets,
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	if err := st.SetCheckAggregate(ctx, "home", true, 1); err != nil {
		t.Fatalf("SetCheckAggregate: %v", err)
	}
	got, err := st.GetCheck(ctx, "home")
	if err != nil {
		t.Fatalf("get check: %v", err)
	}
	if !got.Aggregate || got.MinUp != 1 {
		t.Fatalf("check aggregate = %v min_up = %d, want true/1", got.Aggregate, got.MinUp)
	}

	if err := st.SetCheckAggregate(ctx, "home", true, len(created.Targets)+1); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("min_up above the target count: err = %v, want ErrValidation", err)
	}
	if err := st.SetCheckAggregate(ctx, "ghost", true, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing check: err = %v, want ErrNotFound", err)
	}
}
