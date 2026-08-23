package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// SecretKindAnsibleOutput is the kind bound into the AAD of a sealed ansible
// capture — the twin of SecretKindTofuOutput (tofu_seal.go). It marks the row's
// provenance and, like every kind, is part of what a moved ciphertext fails to
// open against. The AAD shape is unchanged (name|kind); only the kind is new.
const SecretKindAnsibleOutput = "ansible-output"

// AnsibleCaptureDirVar is the extra-var the driver sets to the run's capture
// directory. A play fetches its declared sensitive output there (the mirror of
// how extra-vars are staged into the private dir today), and the driver reads
// the bytes back in-process after the play — never through the streaming event
// sink, because a captured value is a secret.
const AnsibleCaptureDirVar = "exocomp_capture_dir"

// AnsibleCaptureEventKey namespaces the capture summary inside an event's
// raw_json, the way TofuInventoryEventKey namespaces the inventory capture.
const AnsibleCaptureEventKey = "ansible_capture"

// AnsibleOutputBinding is the run-secret binding a workflow's sealed captures
// are collected under. It is the mirror of TofuOutputBinding: a deterministic
// name (ab:<workflow>, ansible-bootstrap) the dependent tofu step names ahead of
// time, so a producing play and a consuming apply agree on it without a
// round-trip. Resolved by that workflow's tofu step and by nothing else.
func AnsibleOutputBinding(workflow string) string {
	return "ab:" + workflow
}

// AnsibleOutputEnvName derives the environment variable name a sealed capture
// binds to: the declared name upper-cased, and nothing else. Case folding is
// not mangling; substituting characters would be, so a name that is not a POSIX
// identifier after folding (a dash, a dot) is refused here rather than rewritten
// into something the operator never wrote. This is the exact rule
// TofuOutputEnvName applies.
func AnsibleOutputEnvName(name string) (string, bool) {
	env := strings.ToUpper(name)
	if !isEnvName(env) {
		return "", false
	}
	return env, true
}

// AnsibleOutputSecretName is the stable run-secret name a sealed capture is
// stored under: ansible-output/<workflow>/<name>. It is deterministic — the
// twin of TofuOutputSecretName — so a re-run reseals the same row and a
// downstream step can name the binding ahead of time.
func AnsibleOutputSecretName(workflow, name string) string {
	return "ansible-output/" + workflow + "/" + name
}

// AnsibleCapture is one operator-declared capture target: an output name (the
// name the sealed row and its env binding derive from) and the file, relative
// to the run's capture dir, the play produced by fetching or copying it. Only a
// declared target is ever captured; an undeclared fact is never scraped — the
// fail-safe direction the tofu path also takes.
type AnsibleCapture struct {
	Name string
	File string
	// AsFile asks the consuming step to receive the sealed value as a FILE (its
	// env rewritten to a 0600 path in the run's tmpfs private tree) rather than as
	// an env value — for a tool that wants a KUBECONFIG file. It is a consumer
	// concern only: the seal is identical either way, so this field never affects
	// how captureAnsibleOutputs reads or seals the value.
	AsFile bool `json:"as_file,omitempty"`
}

// AnsibleCaptureResult is the values-free summary emitted after a capture. It
// carries names only, never a value — the durable event log must stay safe to
// keep (AGENTS.md invariant 4), exactly as InventoryCapture carries names and
// counts.
type AnsibleCaptureResult struct {
	Sealed  []string `json:"sealed,omitempty"`  // declared names sealed into the envelope
	Missing []string `json:"missing,omitempty"` // declared names the play did not produce
	// Unbound reports sealed captures the sealer did not bind to an env name,
	// each with its reason. A name, never a value.
	Unbound []string `json:"unbound,omitempty"`
}

// AnsibleOutputSealer routes a declared ansible capture into the secret store:
// seal under the envelope DEK, persist the ciphertext, then bind it as a named
// env var for the dependent tofu step. It is the mirror of EnvelopeOutputSealer
// (tofu_seal.go); the ansible driver holds only the OutputSealer interface,
// never the store.
//
// Both halves are idempotent by name: a re-run reseals the same
// ansible-output/<workflow>/<name> row (rotate in place) and finds its binding
// pair already present, so the latest run's value is what the next step resolves
// and no re-run duplicates a row.
type AnsibleOutputSealer struct {
	Env   *secrets.Envelope
	Store *store.Store
	// Workflow names the bootstrap chain the captures belong to. It is what
	// makes both names deterministic — the secret row's and the binding's — so a
	// producer and a consumer agree on them without a round-trip.
	Workflow string
}

// SealOutput implements OutputSealer.
func (s AnsibleOutputSealer) SealOutput(ctx context.Context, name string, value []byte) (string, error) {
	if s.Env == nil || s.Store == nil {
		return "", errors.New("executor: ansible output sealer missing envelope or store")
	}
	if s.Workflow == "" {
		return "", errors.New("executor: ansible output sealer missing workflow")
	}
	secretName := AnsibleOutputSecretName(s.Workflow, name)
	sealed, err := s.Env.Seal(secretName, SecretKindAnsibleOutput, value)
	if err != nil {
		return "", fmt.Errorf("executor: seal ansible output: %w", err)
	}
	// Rotate-then-create is the upsert: an existing row (a prior run) rotates in
	// place; a first run creates it. Neither path carries plaintext.
	err = s.Store.RotateSecret(ctx, sealed.Name, sealed.Nonce, sealed.Ciphertext)
	if errors.Is(err, store.ErrNotFound) {
		_, err = s.Store.CreateSecret(ctx, store.SecretRow{
			Name:       sealed.Name,
			Kind:       sealed.Kind,
			Nonce:      sealed.Nonce,
			Ciphertext: sealed.Ciphertext,
			AAD:        sealed.Name + "|" + sealed.Kind,
		})
	}
	if err != nil {
		return "", fmt.Errorf("executor: persist sealed ansible output: %w", err)
	}
	return s.bind(ctx, name, sealed.Name)
}

// bind maps the sealed row to an env name under the workflow's binding, and
// reports why it did not when it did not. A binding is (env name -> secret name)
// only: the value stays in the envelope and is opened per run.
//
// It never overwrites a pair that already points somewhere else. The secret
// name is deterministic, so the only way to reach that branch is a collision an
// operator would not expect — two captures folding to one env name, or their own
// hand-made binding under the same name — and silently repointing KUBECONFIG at
// a different secret is worse than refusing one capture's bind.
func (s AnsibleOutputSealer) bind(ctx context.Context, name, secretName string) (string, error) {
	envName, ok := AnsibleOutputEnvName(name)
	if !ok {
		return name + ": not bound, an env name must be a POSIX identifier", nil
	}
	binding := AnsibleOutputBinding(s.Workflow)
	pairs, err := s.Store.RunSecretsByBinding(ctx, binding)
	if err != nil {
		return "", fmt.Errorf("executor: read binding %q: %w", binding, err)
	}
	for _, p := range pairs {
		if p.EnvName != envName {
			continue
		}
		if p.SecretName == secretName {
			return "", nil
		}
		return fmt.Sprintf("%s: not bound, %s already binds %s", name, envName, p.SecretName), nil
	}
	_, err = s.Store.AddRunSecret(ctx, store.RunSecret{
		Binding:    binding,
		EnvName:    envName,
		SecretName: secretName,
	})
	// A concurrent run of the same workflow can win the insert; the pair it wrote
	// is the one this run would have written, so the bind is done.
	if err != nil && !errors.Is(err, store.ErrDuplicate) {
		return "", fmt.Errorf("executor: bind sealed ansible output: %w", err)
	}
	return "", nil
}

// captureAnsibleOutputs reads each declared capture target from the capture dir
// in-process and seals it, returning a values-free summary. It is the ansible
// twin of the sensitive-output loop in captureTofuInventory: the bytes are read
// straight off disk (a file the play fetched) and never routed through the
// streaming event sink.
//
// A declared target the play did not produce is a report (Missing), not a run
// failure — the play already succeeded and changed hosts (exit-code doctrine),
// so a missing capture surfaces when the dependent step cannot find it, exactly
// as a nil sealer degrades to drop-on-capture on the tofu side.
func captureAnsibleOutputs(ctx context.Context, captureDir string, captures []AnsibleCapture, sealer OutputSealer) (AnsibleCaptureResult, error) {
	var res AnsibleCaptureResult
	for _, c := range captures {
		path := filepath.Join(captureDir, filepath.FromSlash(c.File))
		value, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			res.Missing = append(res.Missing, c.Name)
			continue
		}
		if err != nil {
			return AnsibleCaptureResult{}, fmt.Errorf("executor: read capture %q: %w", c.Name, err)
		}
		reason, err := sealer.SealOutput(ctx, c.Name, value)
		if err != nil {
			return AnsibleCaptureResult{}, fmt.Errorf("executor: seal capture %q: %w", c.Name, err)
		}
		res.Sealed = append(res.Sealed, c.Name)
		if reason != "" {
			res.Unbound = append(res.Unbound, reason)
		}
	}
	sort.Strings(res.Sealed)
	sort.Strings(res.Missing)
	sort.Strings(res.Unbound)
	return res, nil
}

// ansibleCaptureEvent builds the system event announcing a capture: the names
// sealed, the declared targets the play did not produce, and any that were
// sealed but not bound. Names only — the values never travel in an event.
func ansibleCaptureEvent(r AnsibleCaptureResult) Event {
	raw, err := json.Marshal(map[string]AnsibleCaptureResult{AnsibleCaptureEventKey: r})
	if err != nil {
		// Unreachable: the payload is fixed fields and string slices.
		panic("executor: ansible: encode capture: " + err.Error())
	}
	msg := fmt.Sprintf("sealed %d ansible capture(s)", len(r.Sealed))
	if len(r.Missing) > 0 {
		msg += fmt.Sprintf("; %d declared but not produced (%s)", len(r.Missing), strings.Join(r.Missing, ", "))
	}
	if len(r.Unbound) > 0 {
		msg += fmt.Sprintf("; %d not bound to env (%s)", len(r.Unbound), strings.Join(r.Unbound, "; "))
	}
	return Event{Ts: time.Now().UTC(), Level: LevelSystem, Msg: msg, RawJSON: string(raw)}
}
