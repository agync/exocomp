package executor

import (
	"encoding/json"
	"fmt"
	"slices"
)

// ResourceDelta is one resource that moved, named by address with the actions
// tofu would take and the attribute KEYS involved. Values are deliberately
// absent: an attribute value is often a secret or a generated password, a bare
// count is unactionable, and the keys are what tell an operator what changed.
type ResourceDelta struct {
	Address  string   `json:"address"`
	Actions  []string `json:"actions"`
	AttrKeys []string `json:"attr_keys,omitempty"`
}

// TofuPlanOutcome is the structured payload a plan attaches to its event. It
// separates true drift — the world changed under tofu, from `.resource_drift[]`
// in a `-refresh-only` plan — from pending config changes — a `.tf` edit, from
// `.resource_changes[]`. Today's bare `changes` bool conflated the two; the
// JSON already separates them, so the outcome does too. Changes stays
// for readers that only ask "did anything move".
type TofuPlanOutcome struct {
	Operation string          `json:"operation"`
	Changes   bool            `json:"changes"`
	Drift     []ResourceDelta `json:"drift,omitempty"`
	Pending   []ResourceDelta `json:"pending,omitempty"`
}

// noopActions are the change actions that mean "nothing to do", so a resource
// carrying only these is not reported as a delta.
var noopActions = [][]string{{"no-op"}, {"read"}, {}}

// ParsePlanShowJSON reads a `tofu show -json <planfile>` document and pulls out
// the resource-level drift and pending changes with values suppressed. It is
// the load-bearing half of the drift must-fix: the driver runs `show -json` and
// hands the bytes here. A document that does not parse is an error the caller
// treats as "no detail available" rather than a failed run — drift detail
// enriches the outcome, it never decides it (AGENTS.md invariant 3).
func ParsePlanShowJSON(showJSON []byte) (drift, pending []ResourceDelta, err error) {
	var doc struct {
		ResourceDrift   []resourceChange `json:"resource_drift"`
		ResourceChanges []resourceChange `json:"resource_changes"`
	}
	if err := json.Unmarshal(showJSON, &doc); err != nil {
		return nil, nil, fmt.Errorf("executor: tofu: parse show json: %w", err)
	}
	return collectDeltas(doc.ResourceDrift), collectDeltas(doc.ResourceChanges), nil
}

// resourceChange is the slice of tofu's show-json schema this package reads: an
// address, and a change with its actions and the before/after attribute maps
// whose keys — never values — become the delta.
type resourceChange struct {
	Address string `json:"address"`
	Change  struct {
		Actions []string       `json:"actions"`
		Before  map[string]any `json:"before"`
		After   map[string]any `json:"after"`
	} `json:"change"`
}

func collectDeltas(changes []resourceChange) []ResourceDelta {
	var out []ResourceDelta
	for _, c := range changes {
		if slices.ContainsFunc(noopActions, func(n []string) bool {
			return slices.Equal(n, c.Change.Actions)
		}) {
			continue
		}
		out = append(out, ResourceDelta{
			Address:  c.Address,
			Actions:  c.Change.Actions,
			AttrKeys: attrKeys(c.Change.Before, c.Change.After),
		})
	}
	return out
}

// attrKeys is the sorted union of the before and after attribute names. Only
// the names travel; the values stay in the plan file, which is a saved,
// encrypted artifact.
func attrKeys(before, after map[string]any) []string {
	seen := map[string]struct{}{}
	for k := range before {
		seen[k] = struct{}{}
	}
	for k := range after {
		seen[k] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
