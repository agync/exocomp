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
// The passthrough arguments are the operator's own flags — --check, --diff,
// --limit, --tags, -e, --forks — recorded on the job row because they are
// public: what a run does is not a secret, only the vault password and the
// extra-vars values are, and those never travel in argv. They are placed
// before the playbook so ansible-playbook reads them as options.
func AnsibleArgv(playbook, inventory string, passthrough ...string) []string {
	argv := []string{AnsibleBinary, "--inventory"}
	if inventory == "" {
		argv = append(argv, "localhost,", "--connection", "local")
	} else {
		argv = append(argv, inventory)
	}
	argv = append(argv, passthrough...)
	return append(argv, playbook)
}

// AnsibleArtifactArgv builds the argv for a play whose inventory is a
// materialized artifact (story 5): it carries no --inventory of its own,
// because the driver copies AnsibleOptions.InventoryArtifact into the run's own
// 0700 private directory and points ansible at that copy. Keeping the flag out
// of the argv is what makes the private copy the sole inventory — the play
// never names, and the sandbox never grants, the data-dir artifact directly.
func AnsibleArtifactArgv(playbook string, passthrough ...string) []string {
	argv := []string{AnsibleBinary}
	argv = append(argv, passthrough...)
	return append(argv, playbook)
}

// AnsibleOptions configures the driver. Secret material belongs here and not
// in a Spec: specs are persisted, options are resolved per execution.
type AnsibleOptions struct {
	// Binary overrides the executable. Empty resolves ansible-playbook on PATH.
	Binary string
	// VaultID is the --vault-id label. Empty means DefaultVaultID.
	VaultID string
	// VaultPassword is written to the private directory for the run. Empty
	// runs without a vault password. It is the single-vault shorthand for
	// Vaults; when both are set this entry is written first.
	VaultPassword []byte
	// Vaults maps additional --vault-id labels to their passwords, so one repo
	// can carry several vaults (dev/prod) and mix envelope-derived passwords
	// with imported ones (the "dead daemon bricks my playbooks" fix). Each
	// resolves to its own --vault-id label@file.
	Vaults []VaultEntry
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
	// InventoryArtifact is a materialized inventory file (story 5: the
	// non-sensitive tofu outputs a prior apply captured) the run consumes as
	// its inventory. The driver copies it into the run's own 0700 private
	// directory and appends --inventory pointing at that copy, so the play
	// reads an isolated copy and never the data-dir artifact — and its process
	// carries no bearer, no state passphrase, and no tfstate. Empty leaves the
	// argv's own inventory untouched.
	InventoryArtifact string
	// PrivateDirRoot is where the per-job private directory is created.
	// Empty uses the system temp dir.
	PrivateDirRoot string
	// CollectionsPath is appended to ANSIBLE_COLLECTIONS_PATH so a run also
	// searches collections vendored by `playbook vendor` (design/amendments.md
	// item 8) without discarding a path the operator already set. Empty
	// leaves ANSIBLE_COLLECTIONS_PATH exactly as inherited.
	CollectionsPath string
	// Interpreter, when set, is the Python the run's modules execute under:
	// the per-source uv venv's interpreter (ADR 0011). The driver forces it as
	// ansible_python_interpreter through extra-vars so a module resolves the
	// project's declared libraries rather than the bare image interpreter.
	// Empty leaves interpreter discovery to ansible.
	Interpreter string
	// VenvBin, when set, is prepended to PATH so the venv's own console
	// scripts and the interpreter's neighbours resolve first. Empty leaves PATH
	// as the allowlist built it. It pairs with Binary pointing at the venv's
	// ansible-playbook: the whole run executes inside the source's venv.
	VenvBin string
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
	if a.opts.VenvBin != "" {
		spec.Env = prependPath(spec.Env, a.opts.VenvBin)
	}

	// A check-mode run grows a structured, resource-level drift event parsed
	// from its own PLAY RECAP; a normal run streams unchanged. The recap sink
	// only observes the same stdout the operator sees, so the dry run's
	// behaviour and exit code are untouched.
	if isAnsibleCheck(spec.Argv) {
		recap := &recapSink{inner: sink}
		res, err := a.inner.Run(ctx, spec, recap)
		if err == nil {
			emitAnsibleCheckOutcome(recap.snapshot(), sink)
		}
		return res, err
	}
	return a.inner.Run(ctx, spec, sink)
}

// prependPath puts dir at the front of the PATH entry in env so the venv's own
// executables resolve before the image's. A run that inherited no PATH gets one
// naming only dir.
func prependPath(env []string, dir string) []string {
	const prefix = "PATH="
	for i, kv := range env {
		if existing, ok := strings.CutPrefix(kv, prefix); ok {
			env[i] = prefix + dir + string(os.PathListSeparator) + existing
			return env
		}
	}
	return append(env, prefix+dir)
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

// VaultEntry is one --vault-id label and its password. Several may apply to one
// run so a repo's dev and prod vaults resolve at once.
type VaultEntry struct {
	// ID is the --vault-id label. Empty means DefaultVaultID.
	ID string
	// Password is written to the private directory for the run.
	Password []byte
}

// vaultEntries is the effective set: the single-vault shorthand first, then the
// explicit list, so the common one-vault case keeps its stable file name.
func (a *Ansible) vaultEntries() []VaultEntry {
	var entries []VaultEntry
	if len(a.opts.VaultPassword) > 0 {
		entries = append(entries, VaultEntry{ID: a.opts.VaultID, Password: a.opts.VaultPassword})
	}
	return append(entries, a.opts.Vaults...)
}

// writePrivate fills the private directory and returns the arguments pointing
// at it.
func (a *Ansible) writePrivate(dir string) ([]string, error) {
	var args []string

	// The materialized inventory is copied into the private dir before anything
	// else, so the play reads an isolated 0600 copy the run owns. The source is
	// a data-dir artifact the fleet-facing play must never be granted directly.
	if a.opts.InventoryArtifact != "" {
		body, err := os.ReadFile(a.opts.InventoryArtifact)
		if err != nil {
			return nil, fmt.Errorf("executor: ansible: read inventory artifact: %w", err)
		}
		path := filepath.Join(dir, "inventory.json")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return nil, fmt.Errorf("executor: ansible: write inventory: %w", err)
		}
		args = append(args, "--inventory", path)
	}

	seen := map[string]bool{}
	for i, entry := range a.vaultEntries() {
		if len(entry.Password) == 0 {
			continue
		}
		id := entry.ID
		if id == "" {
			id = DefaultVaultID
		}
		if strings.ContainsAny(id, "@ \t\r\n") {
			return nil, fmt.Errorf("executor: ansible: vault id %q contains a separator", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("executor: ansible: vault id %q named twice", id)
		}
		seen[id] = true
		if bytes.ContainsAny(entry.Password, "\r\n") {
			return nil, errors.New("executor: ansible: vault password contains a newline")
		}
		path := filepath.Join(dir, fmt.Sprintf("vault-pass-%d", i))
		pw := append(bytes.Clone(entry.Password), '\n')
		if err := os.WriteFile(path, pw, 0o600); err != nil {
			return nil, fmt.Errorf("executor: ansible: write vault password: %w", err)
		}
		args = append(args, "--vault-id", id+"@"+path)
	}

	// The interpreter is forced through extra-vars, the highest-precedence var
	// source, so it beats any ansible_python_interpreter an inventory or play
	// set: the run's modules must execute under the source's venv, not whatever
	// discovery would otherwise pick (ADR 0011). It merges into the caller's own
	// extra-vars rather than replacing them.
	extraVars := a.opts.ExtraVars
	if a.opts.Interpreter != "" {
		merged := make(map[string]string, len(extraVars)+1)
		for k, v := range extraVars {
			merged[k] = v
		}
		merged["ansible_python_interpreter"] = a.opts.Interpreter
		extraVars = merged
	}
	if len(extraVars) > 0 {
		blob, err := json.Marshal(extraVars)
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
