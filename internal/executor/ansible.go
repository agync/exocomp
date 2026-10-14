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

// AnsibleBinary is the pinned tool the driver runs for a playbook.
const AnsibleBinary = "ansible-playbook"

// AnsibleAdhocBinary is the pinned tool the driver runs for an ad-hoc module
// invocation (`exocomp command` / `exocomp ansible`). It is a sibling of
// ansible-playbook in the same bin directory, so the credential plane, the
// sandbox, and the environment allowlist are shared and only argv[0] differs.
const AnsibleAdhocBinary = "ansible"

// DefaultVaultID labels the vault password when the caller names no label.
const DefaultVaultID = "exocomp"

// ansibleEnvAllow is the environment an ansible run may inherit, alongside
// every ANSIBLE_-prefixed name. Everything else is dropped: a run must not
// pick up whatever the operator happened to export. PYTHONPATH is deliberately
// absent: the venv site-packages bridge is set by the driver after this filter
// (Decision 0016), so an inherited value is dropped rather than merged.
var ansibleEnvAllow = []string{"HOME", "LANG", "LC_ALL", "PATH", "TZ"}

const ansibleEnvPrefix = "ANSIBLE_"

// AnsibleArgv builds the argv a queued ansible job carries. It holds only
// public arguments: vault and extra-vars files live in the per-job private
// directory and are appended when the job runs, so nothing secret and no
// path that outlives the run is written to the job row. An empty inventory
// adds no --inventory at all, so ansible falls back to its own discovery —
// the checkout's ./ansible.cfg `inventory`, then its default hosts file —
// exactly as a bare `ansible-playbook <playbook>` would from that tree.
// The passthrough arguments are the operator's own flags — --check, --diff,
// --limit, --tags, -e, --forks — recorded on the job row because they are
// public: what a run does is not a secret, only the vault password and the
// extra-vars values are, and those never travel in argv. They are placed
// before the playbook so ansible-playbook reads them as options.
func AnsibleArgv(playbook, inventory string, passthrough ...string) []string {
	argv := []string{AnsibleBinary}
	if inventory != "" {
		argv = append(argv, "--inventory", inventory)
	}
	argv = append(argv, passthrough...)
	return append(argv, playbook)
}

// AnsibleArtifactArgv builds the argv for a play whose inventory is the
// handoff-materialized artifact: it carries no --inventory of its own,
// because the driver copies AnsibleOptions.InventoryArtifact into the run's own
// 0700 private directory and points ansible at that copy. Keeping the flag out
// of the argv is what makes the private copy the sole inventory — the play
// never names, and the sandbox never grants, the data-dir artifact directly.
func AnsibleArtifactArgv(playbook string, passthrough ...string) []string {
	argv := []string{AnsibleBinary}
	argv = append(argv, passthrough...)
	return append(argv, playbook)
}

// AnsibleAdhocArgv builds the argv an ad-hoc ansible job carries: the everyday
// operator verb, `ansible <pattern> -i <inv> -m <module> -a "<args>"`, run from
// a source checkout against its inventory. pattern is any ansible host pattern
// (a host, a group, or an expression like `fedora:!nas`) and travels as the sole
// positional. An empty inventory adds no --inventory, so ansible discovers its
// own inventory from the checkout's ./ansible.cfg or default hosts file, the
// same fallback AnsibleArgv uses. module defaults to `command` at the caller;
// args is the module's `-a` string, omitted when empty (ping and setup take
// none). The passthrough carries the same public flags a playbook run does
// (--become, --limit, --check) — secrets keep their own out-of-argv paths, so
// nothing here is a secret and the whole argv is safe on the job row.
func AnsibleAdhocArgv(pattern, inventory, module, args string, passthrough ...string) []string {
	argv := []string{AnsibleAdhocBinary, pattern}
	if inventory != "" {
		argv = append(argv, "--inventory", inventory)
	}
	argv = append(argv, "--module-name", module)
	if args != "" {
		argv = append(argv, "--args", args)
	}
	return append(argv, passthrough...)
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
	// InventoryArtifact is a materialized inventory file (the non-sensitive
	// tofu outputs a prior apply captured) the run consumes as its
	// inventory. The driver copies it into the run's own 0700 private
	// directory and appends --inventory pointing at that copy, so the play
	// reads an isolated copy and never the data-dir artifact — and its process
	// carries no bearer, no state passphrase, and no tfstate. Empty leaves the
	// argv's own inventory untouched.
	InventoryArtifact string
	// PrivateDirRoot is where the per-job private directory is created.
	// Empty uses the system temp dir.
	PrivateDirRoot string
	// CollectionsPath is appended to ANSIBLE_COLLECTIONS_PATH so a run also
	// searches collections vendored by `playbook vendor` (the offline-guarantee
	// amendment) without discarding a path the operator already set. Empty
	// leaves ANSIBLE_COLLECTIONS_PATH exactly as inherited.
	CollectionsPath string
	// Interpreter, when set, is the Python the controller's modules execute
	// under: the per-source uv venv's interpreter (Decision 0011). The driver forces
	// it as ansible_python_interpreter through extra-vars only for the built-in
	// localhost-over-local run, where the controller is the sole host — the venv
	// path is controller-side and would not exist on a remote target. A remote
	// host keeps its own inventory-set or auto-discovered interpreter. Empty
	// leaves interpreter discovery to ansible.
	Interpreter string
	// VenvBin, when set, is prepended to PATH so the venv's own console
	// scripts and the interpreter's neighbours resolve first. Empty leaves PATH
	// as the allowlist built it. It pairs with Binary pointing at the venv's
	// ansible-playbook: the whole run executes inside the source's venv.
	VenvBin string
	// PythonPath, when set, is set as PYTHONPATH so the project venv's
	// site-packages is importable by the PATH-resolved fallback ansible — the
	// Decision 0016 bridge that lets a project's libraries reach the controller's
	// interpreter. Empty leaves PYTHONPATH unset. It is a controller-side
	// bridge: ansible_python_interpreter is target-side only and does not
	// substitute for it.
	PythonPath string
	// TolerateUnreachable opts this run into the degraded outcome: when the only
	// non-success in the PLAY RECAP is unreachable hosts (failed=0 on every host
	// the run reached), the run settles as a tolerated degradation rather than a
	// failure — the online hosts converged and the offline ones are recorded and
	// named, not paged. A task failure on any reachable host stays a hard failure
	// regardless. Off (the default) keeps strict exit-code truth: an unreachable
	// host is a failure like any non-zero exit (AGENTS.md invariant 3 holds; this
	// is a per-run opt-in the operator asked for, not a global reclassification).
	TolerateUnreachable bool
	// Captures are the operator-declared capture targets sealed after a
	// successful play — the ansible mirror of a sensitive tofu output. Each names
	// an output and the file, under the run's capture dir, the play fetched it
	// into. The driver exports that dir as the AnsibleCaptureDirVar extra-var so
	// the play knows where to write. Empty captures nothing; only a declared
	// target is ever sealed (default-deny).
	Captures []AnsibleCapture
	// OutputSealer, when set, seals each declared capture into the secret store
	// and binds it as a named env run-secret for the dependent tofu step. A nil
	// sealer drops the capture — the fail-safe direction the tofu path also takes
	// (tofu_output.go: a nil OutputSealer suppresses rather than leaks). Resolved
	// by the caller from the envelope; the driver never reads the store.
	OutputSealer OutputSealer
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

	// A capture-bearing run gets a dedicated dir under the private tree the play
	// fetches its declared outputs into, and the driver reads back in-process
	// after the play. It is a child of the granted private dir, so the sandboxed
	// play may write there while the value never leaves the process boundary.
	captureDir := ""
	if a.captureActive() {
		captureDir = filepath.Join(private, "capture")
		if err := os.Mkdir(captureDir, 0o700); err != nil {
			return Result{}, fmt.Errorf("executor: ansible: capture dir: %w", err)
		}
	}

	extra, err := a.writePrivate(private, localOnlyTarget(spec.Argv), captureDir)
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
	if a.opts.PythonPath != "" {
		spec.Env = prependEnvList(spec.Env, pythonPathEnv, a.opts.PythonPath)
	}

	// Any run is observed for a host-key mismatch: a connection whose recorded
	// pin no longer matches is a security signal surfaced as an event, so the
	// caller quarantines the host and requires an approved re-pin. The observer
	// keeps only the banner lines; the run's streaming and exit code are
	// untouched (invariant 3).
	//
	// The no-hosts observer sits beneath it so it sees every streamed event: a
	// run whose inventory matched zero hosts exits 0 with an empty recap, and
	// this is what surfaces that as a warning instead of a silent green.
	nh := &noHostsObserver{inner: sink}
	hk := &hostKeyObserver{inner: nh}
	runSink := EventSink(hk)

	// A check-mode run grows a structured drift event, and a tolerate-unreachable
	// run needs the recap to tell an unreachable-only degradation from a real
	// failure. Either reason retains the stdout so the PLAY RECAP can be parsed
	// after the run; a plain run streams unchanged with nothing retained.
	checkMode := isAnsibleCheck(spec.Argv)
	var recap *recapSink
	if checkMode || a.opts.TolerateUnreachable {
		recap = &recapSink{inner: hk}
		runSink = recap
	}

	res, err := a.inner.Run(ctx, spec, runSink)
	if err == nil && checkMode {
		emitAnsibleCheckOutcome(recap.snapshot(), sink)
	}
	if err == nil && a.opts.TolerateUnreachable {
		a.classifyDegraded(&res, recap.snapshot(), sink)
	}
	// A clean run that matched no hosts is warned about, not failed: the exit code
	// stays whatever ansible returned (invariant 3), and only exit 0 can be the
	// silent-green case this guards.
	if err == nil && res.ExitCode == 0 && nh.matchedNoHosts() {
		emitNoHostsOutcome(sink)
	}
	if m, ok := hk.mismatch(); ok {
		sink.Emit(hostKeyMismatchEvent(m))
	}
	// A successful play seals its declared captures — the item-D mirror. It runs
	// after the process exits and before the deferred private-dir cleanup, in the
	// daemon (unsandboxed), reading the fetched bytes off disk exactly as
	// captureTofuInventory reads tofu output in-process. A capture failure is loud
	// but never fails the run: the play already changed hosts (exit-code doctrine).
	if err == nil && res.ExitCode == 0 && captureDir != "" {
		a.captureOutputs(ctx, captureDir, sink)
	}
	return res, err
}

// captureActive reports whether this run has both a sealer and a declared
// target, the two conditions a capture needs. Without a sealer the captures
// drop (default-deny); without a target there is nothing to seal.
func (a *Ansible) captureActive() bool {
	return a.opts.OutputSealer != nil && len(a.opts.Captures) > 0
}

// captureOutputs seals the run's declared captures and emits the values-free
// summary. A failure is surfaced as an error event and swallowed: the play's
// exit code is the verdict, and a missing capture surfaces when the dependent
// step cannot resolve it, not as a false play failure.
func (a *Ansible) captureOutputs(ctx context.Context, captureDir string, sink EventSink) {
	res, err := captureAnsibleOutputs(ctx, captureDir, a.opts.Captures, a.opts.OutputSealer)
	if err != nil {
		sink.Emit(Event{
			Ts:    time.Now().UTC(),
			Level: LevelError,
			Msg:   fmt.Sprintf("ansible capture failed: %v", err),
		})
		return
	}
	sink.Emit(ansibleCaptureEvent(res))
}

// classifyDegraded settles an unreachable-tolerant run whose only non-success is
// unreachable hosts as a tolerated degradation: it marks the result Degraded,
// names the offline hosts, and emits the warning outcome event. It fires only on
// a non-zero exit (a clean run needs no reclassification) and fails closed — no
// recap, or any task-failed host, leaves the result a hard failure untouched, so
// a real failure never hides behind the flag.
func (a *Ansible) classifyDegraded(res *Result, lines []string, sink EventSink) {
	if res.ExitCode == 0 {
		return
	}
	class, found := ClassifyOutcome(lines)
	if !found || len(class.Failed) > 0 || len(class.Unreachable) == 0 {
		return
	}
	// A run that could not reach ANY addressed host is not a degradation: with
	// every host offline nothing converged, so the tolerance flag must not turn
	// a full miss into a tolerated success — it falls through to StateFailed.
	// len(hosts) is the addressed set from the recap, so --limit narrows it.
	hosts, found := ParseRecapHosts(lines)
	if found && len(hosts) > 0 && len(class.Unreachable) == len(hosts) {
		return
	}
	res.Degraded = true
	res.UnreachableHosts = class.Unreachable
	emitPartialOutcome(class.Unreachable, sink)
}

// pythonPathEnv is the environment variable the venv site-packages bridge is
// carried on (Decision 0016).
const pythonPathEnv = "PYTHONPATH"

// prependPath puts dir at the front of the PATH entry in env so the venv's own
// executables resolve before the image's. A run that inherited no PATH gets one
// naming only dir.
func prependPath(env []string, dir string) []string {
	return prependEnvList(env, "PATH", dir)
}

// prependEnvList puts value at the front of the named colon-joined entry in
// env, preserving whatever the entry already carried. A run with no such entry
// gets one naming only value. It is the shared shape behind PATH (the venv's
// bin) and PYTHONPATH (the venv's site-packages bridge, Decision 0016).
func prependEnvList(env []string, name, value string) []string {
	prefix := name + "="
	for i, kv := range env {
		if existing, ok := strings.CutPrefix(kv, prefix); ok {
			env[i] = prefix + value + string(os.PathListSeparator) + existing
			return env
		}
	}
	return append(env, prefix+value)
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
	base := filepath.Base(spec.Argv[0])
	if base != AnsibleBinary && base != AnsibleAdhocBinary {
		return "", fmt.Errorf("executor: ansible: argv[0] is %q, want %s or %s",
			spec.Argv[0], AnsibleBinary, AnsibleAdhocBinary)
	}
	// Binary overrides the playbook executable (the venv's ansible-playbook). The
	// ad-hoc `ansible` is its sibling in the same bin directory, so an ad-hoc run
	// under a venv resolves against that directory rather than PATH.
	if a.opts.Binary != "" {
		if base == AnsibleBinary {
			return a.opts.Binary, nil
		}
		return filepath.Join(filepath.Dir(a.opts.Binary), base), nil
	}
	resolved, err := exec.LookPath(base)
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

// localOnlyTarget reports whether argv's sole inventory is an explicit
// `localhost,` inline target. Only then is every host the controller itself, so
// the source venv interpreter may be forced for all of them. An empty inventory
// no longer maps here: it carries no --inventory at all (ansible discovers its
// own, and its implicit localhost already runs under the venv python that
// ansible itself runs as), so it returns false and the interpreter is left to
// ansible's discovery — as does a real inventory (a path, or the materialized
// artifact that carries no inline --inventory), whose hosts own their own
// interpreter, never the controller's venv path.
func localOnlyTarget(argv []string) bool {
	inventories := inventoryArgs(argv)
	return len(inventories) == 1 && inventories[0] == "localhost,"
}

// inventoryArgs returns every inventory value argv names through -i/--inventory,
// in the order they appear.
func inventoryArgs(argv []string) []string {
	var inventories []string
	for i := 0; i < len(argv); i++ {
		switch {
		case argv[i] == "--inventory" || argv[i] == "-i":
			if i+1 < len(argv) {
				inventories = append(inventories, argv[i+1])
			}
		case strings.HasPrefix(argv[i], "--inventory="):
			inventories = append(inventories, strings.TrimPrefix(argv[i], "--inventory="))
		case strings.HasPrefix(argv[i], "-i") && len(argv[i]) > 2:
			inventories = append(inventories, argv[i][2:])
		}
	}
	return inventories
}

// localConnectionTarget reports whether argv reaches the controller itself over
// a local connection: an explicit `localhost,` inline inventory (localOnlyTarget),
// or an ad-hoc run whose sole host pattern is a loopback name and names no
// inventory. Only then does become's sudo run inside the sandbox rather than on
// a remote host, so the elevation hint can name the connection type as the cause
// instead of blaming the task.
func localConnectionTarget(argv []string) bool {
	if localOnlyTarget(argv) {
		return true
	}
	if len(argv) < 2 || filepath.Base(argv[0]) != AnsibleAdhocBinary {
		return false
	}
	switch argv[1] {
	case "localhost", "127.0.0.1", "::1":
		return len(inventoryArgs(argv)) == 0
	}
	return false
}

// writePrivate fills the private directory and returns the arguments pointing
// at it. localOnly gates whether the source venv interpreter is forced on every
// host (see localOnlyTarget): the controller's venv python is only ever a valid
// interpreter when the controller is the only target. captureDir, when set, is
// exported to the play through the AnsibleCaptureDirVar extra-var so it fetches
// its declared sensitive outputs there.
func (a *Ansible) writePrivate(dir string, localOnly bool, captureDir string) ([]string, error) {
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

	// The interpreter is forced through extra-vars only for the localhost-over-
	// local run, where the controller is the only host: the source venv python is
	// a controller-side path, so forcing it globally on a run that reaches a remote
	// host makes that host's modules resolve a python that does not exist there
	// (rc 127). A remote host takes its interpreter from the inventory or ansible's
	// own discovery; a controller-side task delegated to localhost still resolves
	// the venv because ansible itself runs under it (Decision 0011). The forced value
	// merges into the caller's own extra-vars rather than replacing them.
	extraVars := a.opts.ExtraVars
	forceInterpreter := a.opts.Interpreter != "" && localOnly
	if forceInterpreter || captureDir != "" {
		merged := make(map[string]string, len(extraVars)+2)
		for k, v := range extraVars {
			merged[k] = v
		}
		if forceInterpreter {
			merged["ansible_python_interpreter"] = a.opts.Interpreter
		}
		// The play fetches its declared captures into this dir; the driver reads
		// them back in-process after the run. A driver-set path, never operator
		// input, so it is a plain string var.
		if captureDir != "" {
			merged[AnsibleCaptureDirVar] = captureDir
		}
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
