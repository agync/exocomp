package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// KindAnsible is the Spec kind this driver serves.
const KindAnsible = "ansible"

// AnsibleBinary is the pinned tool the driver runs.
const AnsibleBinary = "ansible-playbook"

// DefaultVaultID labels the vault password when the caller names no label.
const DefaultVaultID = "exocomp"

// ansibleEnvAllow is the environment an ansible run may inherit, alongside
// every ANSIBLE_-prefixed name. Everything else is dropped: a run must not
// pick up whatever the operator happened to export.
var ansibleEnvAllow = []string{"HOME", "LANG", "LC_ALL", "PATH", "TZ"}

const ansibleEnvPrefix = "ANSIBLE_"

// AnsibleArgv builds the argv a queued ansible job carries. It holds only
// public arguments: vault and extra-vars files live in the per-job private
// directory and are appended when the job runs, so nothing secret and no
// path that outlives the run is written to the job row. An empty inventory
// targets localhost over the local connection.
func AnsibleArgv(playbook, inventory string) []string {
	if inventory == "" {
		return []string{AnsibleBinary, "--inventory", "localhost,", "--connection", "local", playbook}
	}
	return []string{AnsibleBinary, "--inventory", inventory, playbook}
}

// AnsibleOptions configures the driver. Secret material belongs here and not
// in a Spec: specs are persisted, options are resolved per execution.
type AnsibleOptions struct {
	// Binary overrides the executable. Empty resolves ansible-playbook on PATH.
	Binary string
	// VaultID is the --vault-id label. Empty means DefaultVaultID.
	VaultID string
	// VaultPassword is written to the private directory for the run. Empty
	// runs without a vault password.
	VaultPassword []byte
	// ExtraVars are passed through a file, never on the command line where
	// the process table would expose them.
	ExtraVars map[string]string
	// Env adds explicit KEY=VALUE entries on top of the inherited allowlist.
	Env []string
	// RunSecrets is the credential-plane binding: sealed-secret values exported
	// as named env for this run alone, so a dynamic-inventory plugin
	// authenticates under the sandbox from a bound token the global allowlist
	// would otherwise strip. Resolved by the caller from the envelope; the
	// driver only injects it.
	RunSecrets RunSecrets
	// PrivateDirRoot is where the per-job private directory is created.
	// Empty uses the system temp dir.
	PrivateDirRoot string
	// CollectionsPath is appended to ANSIBLE_COLLECTIONS_PATH so a run also
	// searches collections vendored by `playbook vendor` (design/amendments.md
	// item 8) without discarding a path the operator already set. Empty
	// leaves ANSIBLE_COLLECTIONS_PATH exactly as inherited.
	CollectionsPath string
}

// Ansible runs playbooks through ansible-playbook. It builds the invocation
// and the environment, then hands the process to the local shell driver:
// streaming and exit-code handling have one implementation.
type Ansible struct {
	opts  AnsibleOptions
	inner Driver
}

// NewAnsible returns the ansible-playbook driver.
func NewAnsible(opts AnsibleOptions) *Ansible {
	return &Ansible{opts: opts, inner: NewShell()}
}

// Run executes the playbook named by spec.Argv. Vault password and extra vars
// are written to a 0700 directory that is removed when the process ends.
func (a *Ansible) Run(ctx context.Context, spec Spec, sink EventSink) (Result, error) {
	bin, err := a.binary(spec)
	if err != nil {
		return Result{}, err
	}

	private, err := os.MkdirTemp(a.opts.PrivateDirRoot, "exocomp-ansible-")
	if err != nil {
		return Result{}, fmt.Errorf("executor: ansible: private dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(private); rmErr != nil {
			sink.Emit(Event{
				Ts:    time.Now().UTC(),
				Level: LevelSystem,
				Msg:   fmt.Sprintf("private dir %s not removed: %v", private, rmErr),
			})
		}
	}()
	if err := os.Chmod(private, 0o700); err != nil {
		return Result{}, fmt.Errorf("executor: ansible: private dir mode: %w", err)
	}

	extra, err := a.writePrivate(private)
	if err != nil {
		return Result{}, err
	}

	argv := make([]string, 0, len(spec.Argv)+len(extra))
	argv = append(argv, bin)
	argv = append(argv, spec.Argv[1:]...)
	argv = append(argv, extra...)

	explicit, err := a.explicitEnv()
	if err != nil {
		return Result{}, err
	}

	spec.Argv = argv
	spec.Env = ansibleEnv(os.Environ(), explicit, a.opts.CollectionsPath)
	return a.inner.Run(ctx, spec, sink)
}

// explicitEnv is the caller's own env entries plus the credential-plane
// binding, the run secrets appended last so a bound provider variable wins over
// an incidental one the operator set by hand.
func (a *Ansible) explicitEnv() ([]string, error) {
	secretEnv, err := runSecretEnv(a.opts.RunSecrets)
	if err != nil {
		return nil, err
	}
	if len(secretEnv) == 0 {
		return a.opts.Env, nil
	}
	out := make([]string, 0, len(a.opts.Env)+len(secretEnv))
	out = append(out, a.opts.Env...)
	return append(out, secretEnv...), nil
}

// binary validates the spec and resolves the executable to run.
func (a *Ansible) binary(spec Spec) (string, error) {
	if spec.Kind != "" && spec.Kind != KindAnsible {
		return "", fmt.Errorf("executor: ansible: spec kind is %q, want %s", spec.Kind, KindAnsible)
	}
	if len(spec.Argv) == 0 {
		return "", errors.New("executor: ansible: empty argv")
	}
	if filepath.Base(spec.Argv[0]) != AnsibleBinary {
		return "", fmt.Errorf("executor: ansible: argv[0] is %q, want %s", spec.Argv[0], AnsibleBinary)
	}
	if a.opts.Binary != "" {
		return a.opts.Binary, nil
	}
	resolved, err := exec.LookPath(AnsibleBinary)
	if err != nil {
		return "", fmt.Errorf("executor: ansible: %w", err)
	}
	return resolved, nil
}

// writePrivate fills the private directory and returns the arguments pointing
// at it.
func (a *Ansible) writePrivate(dir string) ([]string, error) {
	var args []string

	if len(a.opts.VaultPassword) > 0 {
		id := a.opts.VaultID
		if id == "" {
			id = DefaultVaultID
		}
		if strings.ContainsAny(id, "@ \t\r\n") {
			return nil, fmt.Errorf("executor: ansible: vault id %q contains a separator", id)
		}
		if bytes.ContainsAny(a.opts.VaultPassword, "\r\n") {
			return nil, errors.New("executor: ansible: vault password contains a newline")
		}
		path := filepath.Join(dir, "vault-pass")
		pw := append(bytes.Clone(a.opts.VaultPassword), '\n')
		if err := os.WriteFile(path, pw, 0o600); err != nil {
			return nil, fmt.Errorf("executor: ansible: write vault password: %w", err)
		}
		args = append(args, "--vault-id", id+"@"+path)
	}

	if len(a.opts.ExtraVars) > 0 {
		blob, err := json.Marshal(a.opts.ExtraVars)
		if err != nil {
			return nil, fmt.Errorf("executor: ansible: encode extra vars: %w", err)
		}
		path := filepath.Join(dir, "extra-vars.json")
		if err := os.WriteFile(path, blob, 0o600); err != nil {
			return nil, fmt.Errorf("executor: ansible: write extra vars: %w", err)
		}
		args = append(args, "--extra-vars", "@"+path)
	}

	return args, nil
}

// ansibleCollectionsPathVar is the variable ansible-playbook reads for extra
// collection search directories.
const ansibleCollectionsPathVar = "ANSIBLE_COLLECTIONS_PATH"

// ansibleEnv builds the child environment from an allowlist over parent plus
// the caller's explicit entries, which win over what was inherited.
// collectionsPath, if set, is appended to ANSIBLE_COLLECTIONS_PATH rather
// than replacing whatever the operator's own environment already named.
func ansibleEnv(parent, explicit []string, collectionsPath string) []string {
	out := make([]string, 0, len(ansibleEnvAllow)+len(explicit)+1)
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if slices.Contains(ansibleEnvAllow, name) || strings.HasPrefix(name, ansibleEnvPrefix) {
			out = append(out, kv)
		}
	}
	out = append(out, explicit...)
	if collectionsPath == "" {
		return out
	}
	return appendCollectionsPath(out, collectionsPath)
}

// appendCollectionsPath adds dir to the last ANSIBLE_COLLECTIONS_PATH entry
// in env (parent or explicit may have set one), or adds a new entry when
// none did.
func appendCollectionsPath(env []string, dir string) []string {
	prefix := ansibleCollectionsPathVar + "="
	for i := len(env) - 1; i >= 0; i-- {
		if existing, ok := strings.CutPrefix(env[i], prefix); ok {
			env[i] = prefix + existing + string(os.PathListSeparator) + dir
			return env
		}
	}
	return append(env, prefix+dir)
}
