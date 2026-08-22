package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// TofuInventoryEventKey namespaces the inventory-capture summary inside an
// event's raw_json, the way tofuEventKey namespaces the plan outcome.
const TofuInventoryEventKey = "tofu_inventory"

// InventoryOutputName is the canonical output an operator shapes their hosts
// and groups through: a declared output of this name whose value is an object
// IS the materialized inventory (an Ansible JSON/YAML inventory structure).
// With no such output the capture falls back to exposing every non-sensitive
// output as an `all` group var — valid, but hostless — which is why the
// convention is documented (operations/terraform-ansible-handoff.md).
const InventoryOutputName = "ansible_inventory"

// tofuOutput is one entry of `tofu output -json`. Only the sensitivity flag and
// the raw value are read: the declared type is tofu's business, not the
// inventory's.
type tofuOutput struct {
	Value     json.RawMessage `json:"value"`
	Sensitive bool            `json:"sensitive"`
}

// InventoryCapture is the non-sensitive summary emitted after a capture. It
// carries output NAMES only, never a value: an output may hold a secret even
// when it is not sensitive-marked, and the durable event log must stay safe to
// keep regardless. The host and group data lives in the materialized file
// (0600, under the data dir), not here.
type InventoryCapture struct {
	Path       string   `json:"path"`
	SHA256     string   `json:"sha256"`
	Size       int64    `json:"size"`
	Captured   []string `json:"captured"`   // non-sensitive output names written
	Suppressed []string `json:"suppressed"` // sensitive output names dropped (no sealer)
	Sealed     []string `json:"sealed"`     // sensitive output names sealed as run-secrets
}

// OutputSealer seals one sensitive tofu output so a downstream step can bind it
// as a named-env run secret — the secrets twin of the inventory capture. The
// value is plaintext held only for the duration of the call; an implementation
// persists it sealed and MUST NOT log, event, or otherwise surface it. A nil
// sealer restores the drop-on-capture behaviour: a sensitive output then lands
// in Suppressed and reaches nothing.
type OutputSealer interface {
	SealOutput(ctx context.Context, name string, value []byte) error
}

// TofuOutputSecretName is the stable run-secret name a sealed sensitive output
// is stored under: tf-output/<workspace>/<output>. It is deterministic so a
// re-apply reseals the same row and a downstream step can name the binding
// ahead of time.
func TofuOutputSecretName(workspace, output string) string {
	return "tf-output/" + workspace + "/" + output
}

// sensitiveOutputValue is the plaintext to seal for a sensitive output: the
// unquoted string when the output is a JSON string (a token, a password — the
// common case a provider env or TF_VAR consumes verbatim), otherwise the raw
// JSON so a structured value survives intact.
func sensitiveOutputValue(raw json.RawMessage) []byte {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []byte(s)
	}
	return append([]byte(nil), raw...)
}

// captureTofuInventory runs `tofu output -json` in the same environment the
// apply ran under — the one holding the backend bearer and the pbkdf2 state
// passphrase — keeps only the declared, NON-sensitive outputs, shapes them into
// an Ansible inventory, and writes it to outPath (0600, parent 0700).
//
// The governing rule (docs/design/v1-acceptance.md): the inventory is built
// from declared output blocks, never the state file, and a sensitive-marked
// output is dropped before anything is written or emitted. The raw tfstate is
// never read here; `tofu output` returns only what the module declared.
//
// It runs the read directly rather than through the streaming inner driver on
// purpose: that driver turns stdout into durable job events, and `tofu output
// -json` prints every value — sensitive ones included — so its stdout must be
// captured privately and filtered before any of it can become an event. This
// mirrors driftShower, which likewise reads structured output in-process.
func captureTofuInventory(ctx context.Context, bin, dir string, env []string, outPath, workspace string, sealer OutputSealer) (InventoryCapture, error) {
	cmd := exec.CommandContext(ctx, bin, "output", "-json", "-no-color")
	cmd.Dir = dir
	cmd.Env = env
	raw, err := cmd.Output()
	if err != nil {
		return InventoryCapture{}, fmt.Errorf("executor: tofu output: %w", err)
	}

	var outputs map[string]tofuOutput
	if err := json.Unmarshal(raw, &outputs); err != nil {
		return InventoryCapture{}, fmt.Errorf("executor: parse tofu output: %w", err)
	}

	nonSensitive := make(map[string]json.RawMessage, len(outputs))
	var captured, suppressed, sealed []string
	for name, out := range outputs {
		if out.Sensitive {
			// A sensitive output never reaches the inventory or an event. With a
			// sealer it is routed into the envelope as a run-secret for a
			// downstream bind; without one it is dropped, as before.
			if sealer != nil {
				if err := sealer.SealOutput(ctx, TofuOutputSecretName(workspace, name), sensitiveOutputValue(out.Value)); err != nil {
					return InventoryCapture{}, fmt.Errorf("executor: seal sensitive output %q: %w", name, err)
				}
				sealed = append(sealed, name)
			} else {
				suppressed = append(suppressed, name)
			}
			continue
		}
		nonSensitive[name] = out.Value
		captured = append(captured, name)
	}
	sort.Strings(captured)
	sort.Strings(suppressed)
	sort.Strings(sealed)

	body, err := shapeInventory(nonSensitive)
	if err != nil {
		return InventoryCapture{}, err
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return InventoryCapture{}, fmt.Errorf("executor: inventory dir: %w", err)
	}
	if err := os.WriteFile(outPath, body, 0o600); err != nil {
		return InventoryCapture{}, fmt.Errorf("executor: write inventory: %w", err)
	}

	sum := sha256.Sum256(body)
	return InventoryCapture{
		Path:       outPath,
		SHA256:     hex.EncodeToString(sum[:]),
		Size:       int64(len(body)),
		Captured:   captured,
		Suppressed: suppressed,
		Sealed:     sealed,
	}, nil
}

// shapeInventory turns the non-sensitive outputs into an Ansible-consumable
// inventory. The canonical convention: an output named ansible_inventory whose
// value is an object is written as the inventory verbatim, so the operator
// shapes hosts and groups in Terraform. Otherwise every non-sensitive output
// becomes a var on the implicit `all` group — a valid inventory with no hosts,
// which is the documented signal that no ansible_inventory output was declared.
//
// The bytes are canonical JSON (Go marshals map keys sorted), which Ansible's
// auto/yaml inventory plugin parses since JSON is a subset of YAML.
func shapeInventory(nonSensitive map[string]json.RawMessage) ([]byte, error) {
	if raw, ok := nonSensitive[InventoryOutputName]; ok {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err == nil {
			// Re-marshal the parsed object so the on-disk form is canonical and
			// its digest is stable across applies that did not change it.
			body, err := json.MarshalIndent(obj, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("executor: shape inventory: %w", err)
			}
			return append(body, '\n'), nil
		}
		// A non-object ansible_inventory is a misuse, not a secret; fall through
		// to the var form so the mistake surfaces as an empty host list rather
		// than a failed run.
	}

	vars := make(map[string]json.RawMessage, len(nonSensitive))
	for name, v := range nonSensitive {
		vars[name] = v
	}
	body, err := json.MarshalIndent(map[string]any{
		"all": map[string]any{"vars": vars},
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("executor: shape inventory: %w", err)
	}
	return append(body, '\n'), nil
}

// inventoryCaptureEvent builds the system event announcing a capture: the
// materialized path and digest, the non-sensitive names written, and the
// sensitive names dropped. Names only — the values never travel in an event.
func inventoryCaptureEvent(c InventoryCapture) Event {
	raw, err := json.Marshal(map[string]InventoryCapture{TofuInventoryEventKey: c})
	if err != nil {
		// Unreachable: the payload is fixed fields and string slices.
		panic("executor: tofu: encode inventory capture: " + err.Error())
	}
	msg := fmt.Sprintf("captured %d output(s) to inventory %s", len(c.Captured), c.Path)
	if len(c.Sealed) > 0 {
		msg += fmt.Sprintf("; sealed %d sensitive output(s)", len(c.Sealed))
	}
	if len(c.Suppressed) > 0 {
		msg += fmt.Sprintf("; suppressed %d sensitive output(s)", len(c.Suppressed))
	}
	return Event{Ts: time.Now().UTC(), Level: LevelSystem, Msg: msg, RawJSON: string(raw)}
}
