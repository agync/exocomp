package executor

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestShapeInventoryCanonical proves an ansible_inventory output becomes the
// inventory verbatim: host and group structure survives, and the bytes parse
// back to the same object.
func TestShapeInventoryCanonical(t *testing.T) {
	inv := `{"all":{"children":{"web":{"hosts":{"webnode":{}}}},"hosts":{"localhost":{"ansible_connection":"local"}}}}`
	body, err := shapeInventory(map[string]json.RawMessage{
		InventoryOutputName: json.RawMessage(inv),
		"other":             json.RawMessage(`"ignored-when-canonical"`),
	})
	if err != nil {
		t.Fatalf("shapeInventory: %v", err)
	}
	if !bytes.Contains(body, []byte("webnode")) || !bytes.Contains(body, []byte(`"web"`)) {
		t.Errorf("canonical inventory dropped host/group data:\n%s", body)
	}
	// The var-form wrapper must be absent: the canonical output IS the inventory.
	if bytes.Contains(body, []byte(`"vars"`)) || bytes.Contains(body, []byte("ignored-when-canonical")) {
		t.Errorf("canonical inventory wrapped or leaked other outputs:\n%s", body)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("materialized inventory is not valid JSON: %v\n%s", err, body)
	}
	if _, ok := got["all"]; !ok {
		t.Errorf("materialized inventory has no `all` group:\n%s", body)
	}
}

// TestShapeInventoryFallback proves that with no ansible_inventory output every
// non-sensitive output lands as an `all` group var — a valid, hostless
// inventory.
func TestShapeInventoryFallback(t *testing.T) {
	body, err := shapeInventory(map[string]json.RawMessage{
		"region": json.RawMessage(`"eu-north-1"`),
		"count":  json.RawMessage(`3`),
	})
	if err != nil {
		t.Fatalf("shapeInventory: %v", err)
	}
	var got struct {
		All struct {
			Vars map[string]json.RawMessage `json:"vars"`
		} `json:"all"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("fallback inventory is not valid JSON: %v\n%s", err, body)
	}
	if len(got.All.Vars) != 2 || string(got.All.Vars["region"]) != `"eu-north-1"` {
		t.Errorf("fallback inventory lost outputs: %s", body)
	}
}

// TestShapeInventoryEmpty proves a workspace with no non-sensitive outputs still
// yields a valid inventory rather than an error.
func TestShapeInventoryEmpty(t *testing.T) {
	body, err := shapeInventory(nil)
	if err != nil {
		t.Fatalf("shapeInventory: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("empty inventory is not valid JSON: %v\n%s", err, body)
	}
}
