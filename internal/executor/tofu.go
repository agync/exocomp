package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/naming"
)

// KindTofu is the Spec kind this driver serves.
const KindTofu = "tofu"

// TofuBinary is the pinned tool the driver runs.
const TofuBinary = "tofu"

// TofuStatePurpose is the HKDF purpose the state-encryption passphrase is
// derived under; the workspace name is the derivation id. Tofu encrypts its
// own state with it, so there is no second format and nothing extra to lose
// (ADR 0004).
const TofuStatePurpose = "tofu-state"

// BackendOverrideFile is the file the driver drops into a workspace directory
// to point tofu at this control plane's state backend. It is an override file
// so the operator's own configuration never has to mention a backend, and it
// carries no secret: the bearer lives in the private backend configuration
// passed to init.
const BackendOverrideFile = "zz_exocomp_backend_override.tf"

// tofuEnvAllow is the environment a tofu run inherits. TF_ names are not
// inherited wholesale the way ANSIBLE_ names are: TF_CLI_ARGS would let the
// operator's shell rewrite the argv the queue recorded, and TF_ENCRYPTION and
// TF_DATA_DIR are the driver's to set. Only module inputs pass through.
var tofuEnvAllow = []string{"HOME", "LANG", "LC_ALL", "PATH", "TZ"}

const tofuVarPrefix = "TF_VAR_"

// tofuInputOps are the operations that accept -input; `tofu state list` and
// its siblings reject the flag outright.
var tofuInputOps = []string{"init", "plan", "apply", "destroy", "refresh", "validate"}

// tofuSubcommandOps are the operations whose own subcommand is the next
// argument, so generated flags belong after it rather than before.
var tofuSubcommandOps = []string{"state", "workspace", "providers"}

// TofuArgv returns the argv for one tofu operation. The driver adds the flags
// every non-interactive run needs, so a job row records the operation and the
// operator's own arguments and nothing else.
func TofuArgv(op string, args ...string) []string {
	argv := make([]string, 0, len(args)+2)
	argv = append(argv, TofuBinary, op)
	return append(argv, args...)
}

// The payload `tofu plan -detailed-exitcode` reports "changes present" as exit
// code 2, which is neither success nor failure; executor.Result has one exit
// code and the queue reads it as the verdict, so the distinction travels as a
// job event instead — durable, append-only, and readable by whatever asks
// later. TofuPlanOutcome, with its resource-level drift, lives in tofu_drift.go.

// tofuEventKey namespaces the outcome inside an event's raw_json.
const tofuEventKey = "tofu"

// TofuPlanChanges reports whether a job event's raw_json is a plan outcome and
// whether that plan found changes.
func TofuPlanChanges(rawJSON string) (changes, ok bool) {
	outcome, ok := TofuPlanOutcomeFor(rawJSON)
	if !ok {
		return false, false
	}
	return outcome.Changes, true
}

// TofuPlanOutcomeFor reads the full plan outcome back out of a job event's
// raw_json — the detail twin of TofuPlanChanges, for a reader that wants the
// resource-level drift/pending detail (values-free, per ResourceDelta) rather
// than the bare bool. ok is false when the event carries no tofu payload.
func TofuPlanOutcomeFor(rawJSON string) (outcome TofuPlanOutcome, ok bool) {
	if rawJSON == "" {
		return TofuPlanOutcome{}, false
	}
	var envelope struct {
		Tofu *TofuPlanOutcome `json:"tofu"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &envelope); err != nil || envelope.Tofu == nil {
		return TofuPlanOutcome{}, false
	}
	return *envelope.Tofu, true
}

// TofuOptions configures the driver. Like AnsibleOptions it holds the material
// a Spec must not: specs are persisted, options are resolved per execution.
type TofuOptions struct {
	// Binary overrides the executable. Empty resolves tofu on PATH.
	Binary string
	// Workspace names the state this run reads and writes.
	Workspace string
	// BackendURL is the state backend address for that workspace.
	BackendURL string
	// BackendToken is the bearer the backend expects, base64 of the derived
	// subkey. It is written only to the private directory.
	BackendToken string
	// StatePassphrase feeds tofu's own pbkdf2 state encryption. Empty leaves
	// TF_ENCRYPTION unset and state is pushed in the clear.
	StatePassphrase string
	// PrivateDirRoot is where the per-run private directory is created.
	// Empty uses the system temp dir.
	PrivateDirRoot string
	// PluginCacheDir keeps providers across runs, so a run does not need to
	// redownload one a previous run already fetched. Empty leaves tofu's
	// default alone.
	PluginCacheDir string
	// ModuleCacheDir is a persistent per-workspace home for the modules tofu
	// installs on init. TF_DATA_DIR is per-run and ephemeral, so registry and
	// git modules would re-download every init; pointing the run's modules
	// directory at this stable path lets a second init reuse what the first
	// fetched (ADR 0011), the module-plane twin of PluginCacheDir. Empty derives
	// a default beside PluginCacheDir when both a plugin cache and a workspace
	// name are set, and otherwise leaves modules per-run as before.
	ModuleCacheDir string
	// ExtraPorts are outbound TCP ports the operator declared for this
	// workspace, granted additively to the built-in HTTPS-and-backend egress of
	// a sandboxed tofu run. Empty keeps the default posture. It is the plumbing
	// for a homelab whose kubernetes/helm providers must reach an API server on
	// a non-443 port (a kubeadm cluster's 6443). Because Landlock filters by
	// port and not destination, a granted port reaches any host on that port,
	// not only the intended cluster (docs/operations/serve.md).
	ExtraPorts []int
	// OfflineProviders restricts provider resolution to PluginCacheDir alone:
	// no registry, no network (design/amendments.md item 8). Set it only once
	// the cache is known warm; a provider missing from it then fails the run
	// instead of falling back to the network. Requires PluginCacheDir.
	OfflineProviders bool
	// Env adds explicit KEY=VALUE entries on top of the inherited allowlist.
	Env []string
	// RunSecrets is the credential-plane binding: sealed-secret values exported
	// as named env (a provider's CLOUDFLARE_API_TOKEN, PROXMOX_VE_TOKEN, or a
	// TF_VAR_) for this run alone, so mode B runs a provider that needs auth
	// under the sandbox. Resolved by the caller from the envelope; the driver
	// only injects it.
	RunSecrets RunSecrets
	// RunSecretFiles names the RunSecrets whose value must reach the tool as a
	// FILE rather than an env value: each named env is materialized to a 0600
	// file under the resolved secret-staging backend (TmpfsRoot by default,
	// falling back to disk only when tmpfs is unusable — StageRunSecretFiles,
	// ConfigureRunSecretStaging) and its env rewritten to that path, for a
	// tool (kubectl/helm, an exec-auth provider) that wants a KUBECONFIG file
	// (Phase 3, docs/design/bootstrap-first.md). The value never touches argv
	// or an event, and the file is gone once this Run returns.
	RunSecretFiles []string
	// InventoryOut, when set and an apply succeeds, is the path this run writes
	// the materialized Ansible inventory to: the declared, NON-sensitive tofu
	// outputs captured by `tofu output -json` at apply end (story 5). It is the
	// producer half of the Terraform->Ansible handoff. Empty skips capture, so
	// a plain apply is unchanged. The raw tfstate is never the source —
	// declared outputs only, sensitive ones dropped (v1-acceptance.md).
	InventoryOut string
	// OutputSealer, when set, seals each `sensitive = true` tofu output into the
	// envelope as a run-secret named tf-output/<workspace>/<name> instead of
	// dropping it — the secrets twin of the inventory capture. A downstream step
	// then binds the sealed value as named env through the run-secret plane. The
	// sealed value never reaches the inventory or an event. Nil keeps the drop
	// behaviour, so a plain apply is unchanged.
	OutputSealer OutputSealer
}

// Tofu runs OpenTofu against the control plane's own HTTP state backend. It
// generates the backend wiring, sets state encryption, then hands each process
// to the local shell driver so streaming and exit-code handling keep one
// implementation.
type Tofu struct {
	opts  TofuOptions
	inner Driver
}

// NewTofu returns the OpenTofu driver.
func NewTofu(opts TofuOptions) *Tofu {
	return &Tofu{opts: opts, inner: NewShell()}
}

// Run initializes the working directory against the state backend and then
// executes the operation named by spec.Argv. A failing init returns its own
// exit code: the operation never runs against a half-configured backend.
func (t *Tofu) Run(ctx context.Context, spec Spec, sink EventSink) (Result, error) {
	bin, op, err := t.resolve(spec)
	if err != nil {
		return Result{}, err
	}
	if spec.Dir == "" {
		return Result{}, errors.New("executor: tofu: no working directory")
	}

	private, err := os.MkdirTemp(t.opts.PrivateDirRoot, "exocomp-tofu-")
	if err != nil {
		return Result{}, fmt.Errorf("executor: tofu: private dir: %w", err)
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
		return Result{}, fmt.Errorf("executor: tofu: private dir mode: %w", err)
	}

	backendConfig, err := t.writeBackendConfig(private)
	if err != nil {
		return Result{}, err
	}
	removeOverride, err := writeBackendOverride(spec.Dir)
	if err != nil {
		return Result{}, err
	}
	defer removeOverride(sink)

	var cliConfig string
	if t.opts.OfflineProviders {
		if t.opts.PluginCacheDir == "" {
			return Result{}, errors.New("executor: tofu: offline providers requires a plugin cache dir")
		}
		cliConfig, err = writeOfflineProviderConfig(private, t.opts.PluginCacheDir)
		if err != nil {
			return Result{}, err
		}
	}

	dataDir := filepath.Join(private, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("executor: tofu: data dir: %w", err)
	}
	if err := t.linkModuleCache(dataDir, sink); err != nil {
		return Result{}, err
	}

	// A run-secret that must reach the tool as a KUBECONFIG file is staged as a
	// 0600 file under the resolved secret-staging backend (tmpfs by default;
	// disk only when tmpfs is unusable and the backend is auto or disk) and
	// its env rewritten to that path. Unset when a sandboxed caller
	// (sandboxedTofu) has already staged it before this Run started, to grant
	// the sandbox profile the path ahead of time; this is the plain-tier
	// path, exercised directly or as the sandboxed driver's unsandboxed inner
	// call.
	rs := t.opts.RunSecrets
	var staged []StagedRunSecretFile
	if len(t.opts.RunSecretFiles) > 0 {
		rs, staged, err = StageRunSecretFiles(rs, t.opts.RunSecretFiles)
		if err != nil {
			return Result{}, err
		}
		noteDiskStagingResidual(sink, staged)
	}
	defer func() {
		for i := range staged {
			_ = staged[i].Close()
		}
	}()
	env, err := t.env(dataDir, cliConfig, rs)
	if err != nil {
		return Result{}, err
	}

	// One deadline covers init and the operation together; the inner driver
	// would otherwise grant each of them the job's whole timeout.
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	spec.Timeout = 0
	spec.Env = env

	// Offline resolution needs a lockfile the filesystem mirror can satisfy. A
	// lockfile written online carries only zh: (registry) hashes; the mirror
	// provides h1: hashes, so an offline init would fail verification against a
	// zh:-only lock. Regenerate the lock from the mirror for this platform
	// first, so init verifies with no network (expert-reviews.md #6).
	if t.opts.OfflineProviders {
		res, lockErr := t.lockProvidersFromMirror(ctx, bin, spec, sink)
		if lockErr != nil || res.ExitCode != 0 {
			return res, lockErr
		}
	}

	if op != "init" {
		initSpec := spec
		initSpec.Argv = tofuFlags(bin, []string{TofuBinary, "init", "-backend-config=" + backendConfig})
		sink.Emit(Event{Ts: time.Now().UTC(), Level: LevelSystem, Msg: "tofu init"})
		res, err := t.inner.Run(ctx, initSpec, sink)
		if err != nil || res.ExitCode != 0 {
			return res, err
		}
	} else {
		spec.Argv = append(spec.Argv, "-backend-config="+backendConfig)
	}

	spec.Argv = tofuFlags(bin, spec.Argv)
	sink.Emit(Event{Ts: time.Now().UTC(), Level: LevelSystem, Msg: "tofu " + op})

	res, err := t.inner.Run(ctx, spec, sink)
	if err != nil {
		return res, err
	}
	if op == "plan" {
		res = reportPlanOutcome(ctx, res, sink, t.driftShower(bin, spec))
	}
	if op == "apply" && res.ExitCode == 0 && t.opts.InventoryOut != "" {
		t.captureInventory(ctx, bin, spec, sink)
	}
	return res, nil
}

// captureInventory materializes the workspace's declared, non-sensitive tofu
// outputs as an Ansible inventory after a successful apply — the story-5
// producer. It runs while the backend override and the passphrase env are still
// live (their cleanups are deferred), so `tofu output -json` reads the state
// the apply just wrote. A capture failure is loud but never fails the run: the
// apply already succeeded and changed infrastructure (exit-code doctrine), so a
// missing inventory surfaces when the dependent play cannot find it, not as a
// false apply failure.
func (t *Tofu) captureInventory(ctx context.Context, bin string, spec Spec, sink EventSink) {
	capture, err := captureTofuInventory(ctx, bin, spec.Dir, spec.Env, t.opts.InventoryOut, t.opts.OutputSealer)
	if err != nil {
		sink.Emit(Event{
			Ts:    time.Now().UTC(),
			Level: LevelError,
			Msg:   fmt.Sprintf("inventory capture failed: %v", err),
		})
		return
	}
	sink.Emit(inventoryCaptureEvent(capture))
}

// lockProvidersFromMirror runs `tofu providers lock -fs-mirror <dir>
// -platform=<current>`, writing platform-correct h1: hashes into the workspace's
// .terraform.lock.hcl. Sourced from the filesystem mirror with no network, it
// is what lets the following offline init verify: the filesystem_mirror method
// yields h1: hashes, and a lock that already carries them (rather than the zh:
// hashes an online run left) verifies against the mirror with no egress. The
// spec already carries the offline CLI config in its env, so this command reads
// the mirror too, not the registry.
func (t *Tofu) lockProvidersFromMirror(ctx context.Context, bin string, spec Spec, sink EventSink) (Result, error) {
	platform := runtime.GOOS + "_" + runtime.GOARCH
	lockSpec := spec
	lockSpec.Argv = tofuFlags(bin, []string{TofuBinary, "providers", "lock",
		"-fs-mirror=" + t.opts.PluginCacheDir, "-platform=" + platform})
	sink.Emit(Event{Ts: time.Now().UTC(), Level: LevelSystem,
		Msg: "tofu providers lock (offline, " + platform + ")"})
	return t.inner.Run(ctx, lockSpec, sink)
}

// driftShower captures `tofu show -json <planfile>` for a plan that wrote one,
// so the outcome carries resource-level drift. It runs in the daemon, reading
// the plan file the run produced — the file is a local artifact the control
// plane already holds, so no sandbox is needed to describe it, and a failure
// here only omits the detail (the plan's exit code is still the verdict). A
// plan with no -out has no file to describe and returns nothing.
func (t *Tofu) driftShower(bin string, spec Spec) func(context.Context) []byte {
	planFile, _ := planOutPath(spec.Argv)
	if planFile == "" {
		return nil
	}
	return func(ctx context.Context) []byte {
		cmd := exec.CommandContext(ctx, bin, "show", "-json", "-no-color", planFile)
		cmd.Dir = spec.Dir
		cmd.Env = spec.Env
		out, err := cmd.Output()
		if err != nil {
			return nil
		}
		return out
	}
}

// resolve validates the spec and resolves the executable and the operation.
func (t *Tofu) resolve(spec Spec) (bin, op string, err error) {
	if spec.Kind != "" && spec.Kind != KindTofu {
		return "", "", fmt.Errorf("executor: tofu: spec kind is %q, want %s", spec.Kind, KindTofu)
	}
	if len(spec.Argv) < 2 {
		return "", "", errors.New("executor: tofu: argv needs a binary and an operation")
	}
	if filepath.Base(spec.Argv[0]) != TofuBinary {
		return "", "", fmt.Errorf("executor: tofu: argv[0] is %q, want %s", spec.Argv[0], TofuBinary)
	}
	op = spec.Argv[1]
	if strings.HasPrefix(op, "-") {
		return "", "", fmt.Errorf("executor: tofu: argv[1] is %q, want an operation", op)
	}

	bin = t.opts.Binary
	if bin == "" {
		resolved, lookErr := exec.LookPath(TofuBinary)
		if lookErr != nil {
			return "", "", fmt.Errorf("executor: tofu: %w", lookErr)
		}
		bin = resolved
	}
	return bin, op, nil
}

// reportPlanOutcome turns -detailed-exitcode's third answer into an event.
// Exit 2 means the plan found changes, which is a fact about infrastructure,
// not a failed run, so the job's exit code becomes 0. When a shower is given —
// the plan wrote a file to describe — the outcome carries the resource-level
// drift and pending changes, with attribute values suppressed.
func reportPlanOutcome(ctx context.Context, res Result, sink EventSink, show func(context.Context) []byte) Result {
	changes := res.ExitCode == 2
	if res.ExitCode != 0 && !changes {
		return res
	}

	outcome := TofuPlanOutcome{Operation: "plan", Changes: changes}
	if changes && show != nil {
		if body := show(ctx); body != nil {
			if drift, pending, err := ParsePlanShowJSON(body); err == nil {
				outcome.Drift = drift
				outcome.Pending = pending
			}
		}
	}

	raw, err := json.Marshal(map[string]TofuPlanOutcome{tofuEventKey: outcome})
	if err != nil {
		// Unreachable: the payload is fixed fields and string slices.
		panic("executor: tofu: encode plan outcome: " + err.Error())
	}

	sink.Emit(Event{
		Ts:      time.Now().UTC(),
		Level:   LevelSystem,
		Msg:     planOutcomeMsg(outcome),
		RawJSON: string(raw),
	})
	res.ExitCode = 0
	return res
}

// planOutcomeMsg is the human line beside the structured payload: the
// resource-level drift when there is any, a bare "changes present" otherwise.
func planOutcomeMsg(o TofuPlanOutcome) string {
	if !o.Changes {
		return "tofu plan: no changes"
	}
	if len(o.Drift) > 0 {
		addrs := make([]string, len(o.Drift))
		for i, d := range o.Drift {
			addrs[i] = d.Address
		}
		return "tofu plan: drift in " + strings.Join(addrs, ", ")
	}
	return "tofu plan: changes present (exit 2)"
}

// tofuFlags replaces argv[0] with the resolved binary and inserts the flags a
// queued run always needs, leaving any the caller already chose alone.
//
// Position matters twice over: tofu stops reading flags at the first
// positional argument, so `apply <planfile>` needs them before the file, while
// `state list` needs them after the subcommand. The command path is what the
// flags follow.
func tofuFlags(bin string, argv []string) []string {
	op := argv[1]

	depth := 1
	if slices.Contains(tofuSubcommandOps, op) {
		depth = 2
	}
	if depth > len(argv)-1 {
		depth = len(argv) - 1
	}

	var inject []string
	if !slices.Contains(argv, "-no-color") {
		inject = append(inject, "-no-color")
	}
	if slices.Contains(tofuInputOps, op) && !hasFlag(argv, "-input") {
		inject = append(inject, "-input=false")
	}
	if op == "plan" && !slices.Contains(argv, "-detailed-exitcode") {
		inject = append(inject, "-detailed-exitcode")
	}

	out := make([]string, 0, len(argv)+len(inject))
	out = append(out, bin)
	out = append(out, argv[1:1+depth]...)
	out = append(out, inject...)
	return append(out, argv[1+depth:]...)
}

func hasFlag(argv []string, name string) bool {
	for _, a := range argv {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// writeBackendConfig writes the partial backend configuration init consumes.
// It holds the bearer, so it lives in the 0700 private directory and dies with
// it rather than sitting in the operator's workspace directory.
func (t *Tofu) writeBackendConfig(dir string) (string, error) {
	if t.opts.BackendURL == "" {
		return "", errors.New("executor: tofu: no backend url")
	}
	address, err := hclString(t.opts.BackendURL)
	if err != nil {
		return "", fmt.Errorf("executor: tofu: backend url: %w", err)
	}
	token, err := hclString("Bearer " + t.opts.BackendToken)
	if err != nil {
		return "", fmt.Errorf("executor: tofu: backend token: %w", err)
	}

	body := fmt.Sprintf(`address        = %[1]s
lock_address   = %[1]s
unlock_address = %[1]s
lock_method    = "LOCK"
unlock_method  = "UNLOCK"
headers = {
  Authorization = %[2]s
}
`, address, token)

	path := filepath.Join(dir, "backend.hcl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("executor: tofu: write backend config: %w", err)
	}
	return path, nil
}

// writeOfflineProviderConfig writes a CLI configuration that resolves
// providers from pluginCacheDir alone, declaring no "direct" installation
// method at all. OpenTofu's filesystem-mirror method reads the exact
// directory layout TF_PLUGIN_CACHE_DIR already populates, so the one
// directory serves both roles: an online run downloads into it, an offline
// run resolves only from it. A "direct" fallback is deliberately absent —
// tofu queries the registry's discovery document for any provider a
// declared "direct" method covers even when a cached copy would already
// satisfy the request, which defeats offline resolution entirely.
func writeOfflineProviderConfig(dir, pluginCacheDir string) (string, error) {
	path, err := hclString(pluginCacheDir)
	if err != nil {
		return "", fmt.Errorf("executor: tofu: plugin cache dir: %w", err)
	}
	body := fmt.Sprintf(`provider_installation {
  filesystem_mirror {
    path = %s
  }
}
`, path)

	out := filepath.Join(dir, "offline-providers.tfrc")
	if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("executor: tofu: write offline provider config: %w", err)
	}
	return out, nil
}

// writeBackendOverride declares the http backend in the workspace directory,
// which is the only place tofu reads configuration from. The block is empty:
// every value arrives through the private backend configuration.
//
// The file is shared by everything running in that directory, so two runs
// there at once would remove each other's backend mid-flight. The queue's
// concurrency key is what keeps them apart, which is why a workspace owns its
// directory rather than sharing one.
func writeBackendOverride(dir string) (func(EventSink), error) {
	path := filepath.Join(dir, BackendOverrideFile)
	body := "terraform {\n  backend \"http\" {}\n}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return nil, fmt.Errorf("executor: tofu: write backend override: %w", err)
	}
	return func(sink EventSink) {
		if err := os.Remove(path); err != nil {
			sink.Emit(Event{
				Ts:    time.Now().UTC(),
				Level: LevelSystem,
				Msg:   fmt.Sprintf("backend override %s not removed: %v", path, err),
			})
		}
	}, nil
}

// linkModuleCache points the run's per-run modules directory at a persistent
// per-workspace home, so modules a prior init fetched are reused rather than
// re-downloaded (ADR 0011). tofu installs modules at $TF_DATA_DIR/modules; a
// symlink there redirects the writes to the stable cache transparently, and a
// following init finds modules.json already present and skips the fetch.
//
// A cache miss (no derivable location) leaves modules per-run, unchanged from
// before. A symlink failure is a hard error: proceeding would silently re-fetch
// every run, which is exactly what this exists to stop.
func (t *Tofu) linkModuleCache(dataDir string, sink EventSink) error {
	cache := moduleCacheDir(t.opts)
	if cache == "" {
		return nil
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return fmt.Errorf("executor: tofu: module cache dir: %w", err)
	}
	link := filepath.Join(dataDir, "modules")
	if err := os.Symlink(cache, link); err != nil {
		return fmt.Errorf("executor: tofu: link module cache: %w", err)
	}
	sink.Emit(Event{Ts: time.Now().UTC(), Level: LevelSystem,
		Msg: "tofu module cache " + cache})
	return nil
}

// moduleCacheDir resolves the persistent module cache location: the explicit
// option when set, otherwise a directory beside the plugin cache keyed by the
// workspace. It returns "" when nothing can be derived, so a run with neither a
// plugin cache nor a workspace keeps modules per-run. It is a function of the
// options alone so the sandboxed tier grants the same directory the driver
// links.
func moduleCacheDir(opts TofuOptions) string {
	if opts.ModuleCacheDir != "" {
		return opts.ModuleCacheDir
	}
	if opts.PluginCacheDir == "" || !naming.Component(opts.Workspace) {
		return ""
	}
	return filepath.Join(filepath.Dir(opts.PluginCacheDir), "module-cache", opts.Workspace)
}

// env builds the child environment: the allowlist over the parent, then the
// entries that make the run non-interactive, private, and encrypted.
// cliConfig, when set, is the offline provider configuration's path.
func (t *Tofu) env(dataDir, cliConfig string, rs RunSecrets) ([]string, error) {
	out := make([]string, 0, len(tofuEnvAllow)+8)
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if slices.Contains(tofuEnvAllow, name) || strings.HasPrefix(name, tofuVarPrefix) {
			out = append(out, kv)
		}
	}

	out = append(out,
		"TF_DATA_DIR="+dataDir,
		"TF_IN_AUTOMATION=1",
		"CHECKPOINT_DISABLE=1",
	)
	if t.opts.Workspace != "" {
		// The exocomp workspace name IS the state identity, so expose it to the
		// config as var.exocomp_workspace — a config can branch on the environment
		// the way terraform.workspace would under native TF workspaces. The HTTP
		// backend has no native workspace multiplexing, so a var is the mechanism;
		// it is namespaced so it never clobbers a user's own var.workspace, and is
		// a no-op unless the config declares `variable "exocomp_workspace"`.
		out = append(out, "TF_VAR_exocomp_workspace="+t.opts.Workspace)
	}
	if t.opts.PluginCacheDir != "" {
		out = append(out, "TF_PLUGIN_CACHE_DIR="+t.opts.PluginCacheDir)
	}
	if cliConfig != "" {
		out = append(out, "TF_CLI_CONFIG_FILE="+cliConfig)
	}
	if t.opts.StatePassphrase != "" {
		out = append(out, "TF_ENCRYPTION="+encryptionConfig(t.opts.StatePassphrase))
	}
	out = append(out, t.opts.Env...)

	// The bound credentials go on last so a provider variable wins over an
	// incidental one, and after TF_VAR_ inheritance so a bound TF_VAR_ is the
	// one tofu reads. rs is the caller's RunSecrets after any file staging, so a
	// file-staged name here already carries its path, not its value.
	secretEnv, err := runSecretEnv(rs)
	if err != nil {
		return nil, err
	}
	return append(out, secretEnv...), nil
}

// encryptionConfig is the TF_ENCRYPTION document: pbkdf2 over the derived
// passphrase, AES-GCM over the state. Tofu does the encrypting, so there is no
// second format and no third party in the recovery path (ADR 0004).
func encryptionConfig(passphrase string) string {
	quoted, err := hclString(passphrase)
	if err != nil {
		// Unreachable: the passphrase is base64 of a derived subkey.
		panic("executor: tofu: state passphrase: " + err.Error())
	}
	return fmt.Sprintf(`key_provider "pbkdf2" "exocomp" {
  passphrase = %s
}
method "aes_gcm" "exocomp" {
  keys = key_provider.pbkdf2.exocomp
}
state {
  method = method.aes_gcm.exocomp
}
`, quoted)
}

// hclString quotes a value for an HCL document, refusing the characters that
// would end the string or start an interpolation instead of escaping them:
// every value the driver writes is a URL, a token, or a derived passphrase,
// and none of them may legitimately contain any of these.
func hclString(v string) (string, error) {
	if strings.ContainsAny(v, "\"\\\n\r") || strings.Contains(v, "${") || strings.Contains(v, "%{") {
		return "", fmt.Errorf("value %q is not representable in a generated HCL string", v)
	}
	return `"` + v + `"`, nil
}
