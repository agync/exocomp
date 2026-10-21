package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newSandboxCanaryCmd)
}

// sandboxTierEnv selects the isolation tier when no flag names one:
// opportunistic | enforcing | disabled. Enforcing is the default and fails
// closed where the kernel cannot enforce the sandbox (Decision 0005).
const sandboxTierEnv = "EXOCOMP_SANDBOX"

// sandboxCanarySubcommand is the argv the probe re-enters this binary with.
const sandboxCanarySubcommand = "sandbox-canary"

// privateRunDir holds every job's per-run private directory, inside the 0700
// data dir rather than the system temp dir.
const privateRunDir = "run"

// jobRunDir is where sandboxed jobs get their private directories. The probe
// needs it before any driver exists, because its length decides whether the
// tier can work at all.
func jobRunDir() string {
	dir, err := dataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, privateRunDir)
}

// sandbox holds the tier decision. It is resolved once per process: the probe
// starts a subprocess and a listening socket, and every driver built afterwards
// has to agree on the answer.
var sandbox struct {
	once      sync.Once
	requested executor.Tier
	result    executor.ProbeResult
	warn      io.Writer
}

// requestSandboxTier records the tier a flag asked for, and where a
// degradation should be reported. An empty name leaves the environment to
// decide. It has to run before the first driver is built; after that the
// decision is fixed.
func requestSandboxTier(name string, warn io.Writer) error {
	sandbox.warn = warn
	if name == "" {
		return nil
	}
	t, err := executor.ParseTier(name)
	if err != nil {
		return err
	}
	sandbox.requested = t
	return nil
}

// sandboxTier resolves the effective tier, probing at most once. A degradation
// is written to the warning stream — Decision 0005's "never silently" applies to the
// daemon's log as much as to the doctor.
func sandboxTier(ctx context.Context) executor.ProbeResult {
	sandbox.once.Do(func() {
		warn := sandbox.warn
		if warn == nil {
			warn = os.Stderr
		}

		want := sandbox.requested
		if want == "" {
			parsed, err := executor.ParseTier(os.Getenv(sandboxTierEnv))
			if err != nil {
				_, _ = fmt.Fprintf(warn, "WARNING: %v; using %s\n", err, executor.TierEnforcing)
				parsed = executor.TierEnforcing
			}
			want = parsed
		}
		sandbox.requested = want

		self, args := sandboxCanary()
		sandbox.result = executor.Probe(ctx, want, executor.ProbeOptions{
			Canary:      self,
			CanaryArgs:  args,
			PrivateRoot: jobRunDir(),
		})
		if sandbox.result.Degraded() {
			_, _ = fmt.Fprintf(warn,
				"WARNING: job sandbox unavailable — jobs run at the %s tier instead of %s: %s\n",
				executor.TierDisabled, executor.TierEnforcing, sandbox.result.Reason)
		}
	})
	return sandbox.result
}

// sandboxed reports whether jobs run under the kernel sandbox.
func sandboxed(ctx context.Context) bool {
	return sandboxTier(ctx).Effective == executor.TierEnforcing
}

// enforceSandboxTier fails closed when the sandbox was asked for by name and
// the host cannot provide it. TierOpportunistic degrades to the subprocess
// boundary with a warning — that is what "opportunistic" means. An explicit
// enforcing request is a policy, not a preference: running the fleet's jobs
// unsandboxed is not what was asked for, so the daemon and the doctor refuse
// rather than quietly downgrade (Decision 0005, "never silently"). Enforcing is also
// the secure-by-default tier, so an empty flag reaches this same refusal.
func enforceSandboxTier(ctx context.Context) error {
	res := sandboxTier(ctx)
	if res.Requested == executor.TierEnforcing && res.Degraded() {
		return fmt.Errorf("sandbox: %s was requested but cannot be enforced here: %s; "+
			"pass --sandbox %s to accept a loud degradation, or --sandbox %s for no sandbox",
			executor.TierEnforcing, res.Reason, executor.TierOpportunistic, executor.TierDisabled)
	}
	return nil
}

// sandboxCanary names the executable the probe runs inside the sandbox: this
// binary re-entered at its hidden canary subcommand, so what is asserted is the
// same process image jobs are launched by.
func sandboxCanary() (string, []string) {
	self, err := os.Executable()
	if err != nil {
		return "", nil
	}
	return self, []string{sandboxCanarySubcommand}
}

// newSandboxCanaryCmd is the payload the startup probe runs under nono. It is
// hidden because it is a diagnostic rather than an operator command: it reports
// whether the sandbox refused a key read and a unix socket connect.
func newSandboxCanaryCmd() *cobra.Command {
	return &cobra.Command{
		Use:    sandboxCanarySubcommand + " <key> <socket> <abstract-socket> <tcp-addr>",
		Short:  "Report from inside a sandbox which of four forbidden reaches were refused",
		Hidden: true,
		Args:   cobra.ExactArgs(4),
		Run: func(cmd *cobra.Command, args []string) {
			targets, err := executor.ParseCanaryTargets(args)
			if err != nil {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err)
				os.Exit(executor.CanaryBadInvoke)
			}
			os.Exit(executor.RunCanary(cmd.OutOrStdout(), cmd.ErrOrStderr(), targets))
		},
	}
}

// reportRunSecretFileStaging writes the doctor's run-secret-file-staging
// block: which backend a captured value that must reach a tool as a file
// (bootstrap --capture-file) resolves to right now, the tmpfs usability probe
// behind that resolution, the disk residual posture when disk is in play, and
// whether this process can mlock the bytes it writes either way.
func reportRunSecretFileStaging(w io.Writer, dataDir string) {
	backend, warning := configuredSecretStagingBackend(dataDir)
	if warning != "" {
		_, _ = fmt.Fprintf(w, "  WARNING: %s\n", warning)
	}
	printRunSecretFileStaging(w, dataDir, backend)
}

// configuredSecretStagingBackend reads the --secret-staging choice a daemon
// persisted (store.SettingSecretStaging) back from the store, defaulting to
// auto — the same default serve itself uses — when the data dir has no store
// yet, the setting was never written, or a stored value fails to parse. It
// stats the database file before opening a store, so doctor never creates the
// data dir as a side effect of reporting on it
// (TestDoctorDoesNotCreateTheDataDir), the same guard internal/health.
// ProbeLitestream uses. Never fails doctor: a resolution problem is reported
// by printRunSecretFileStaging instead.
//
// This is the only place the persisted setting is read back — serve.go
// writes it from the already-validated --secret-staging flag but never reads
// it for its own operation, so the precedence for what a live daemon actually
// uses is flag > persisted > default, and this function only serves `doctor`
// reporting against a data dir that may belong to a stopped daemon. Because
// ParseStagingBackend rejects anything outside {auto, tmpfs, disk}, a
// corrupted or hand-edited stored value can never resolve to the disk
// backend on its own — only an actually-persisted "disk" can. The returned
// warning is non-empty exactly when a non-empty stored value failed to
// parse, so the caller can surface the corruption instead of resolving to
// the safe default silently.
func configuredSecretStagingBackend(dataDir string) (backend executor.StagingBackend, warning string) {
	if _, err := os.Stat(filepath.Join(dataDir, "exocomp.db")); err != nil {
		return executor.StagingAuto, ""
	}
	st, err := openStore()
	if err != nil {
		return executor.StagingAuto, ""
	}
	defer func() { _ = st.Close() }()
	raw, err := st.GetSetting(context.Background(), store.SettingSecretStaging)
	if err != nil {
		return executor.StagingAuto, ""
	}
	backend, err = executor.ParseStagingBackend(raw)
	if err != nil {
		return executor.StagingAuto, fmt.Sprintf(
			"stored secret staging setting %q is not a known backend (want %s, %s, or %s); using %s",
			raw, executor.StagingAuto, executor.StagingTmpfs, executor.StagingDisk, executor.StagingAuto)
	}
	return backend, ""
}

func printRunSecretFileStaging(w io.Writer, dataDir string, configured executor.StagingBackend) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("run-secret file staging:\n")
	p("  %-18s %s\n", "configured", configured)

	diskRoot := filepath.Join(dataDir, privateRunDir)
	res, err := executor.ResolveStaging(executor.StagingConfig{Backend: configured, DiskRoot: diskRoot})
	switch {
	case err != nil:
		p("  %-18s REFUSED — %v\n", "resolved", err)
	case res.FellBack:
		p("  %-18s %s at %s — auto asked for tmpfs and fell back to disk (LOUD, not silent): %s\n",
			"resolved", res.Backend, res.Root, res.TmpfsReason)
	default:
		p("  %-18s %s at %s\n", "resolved", res.Backend, res.Root)
	}
	if err == nil {
		if res.TmpfsUsable {
			p("  %-18s usable (%s)\n", "tmpfs", executor.TmpfsRoot)
		} else {
			p("  %-18s unusable — %s\n", "tmpfs", res.TmpfsReason)
		}
		if res.Backend == executor.StagingDisk {
			p("  %-18s 0600 file, 0700 dir, best-effort mlock, excluded from backup (docs/operations/serve.md)\n",
				"disk residual")
		}
	}

	lock := secrets.ProbeMemoryLock()
	if lock.Locked {
		p("  %-18s available — a staged secret's bytes are pinned out of swap\n", "memlock")
	} else {
		p("  %-18s NOT available (%v) — a staged secret proceeds unlocked (best-effort residual, not a refusal)\n",
			"memlock", lock.Err)
	}
}

// reportCaps writes the doctor's per-job caps block: whether a delegated,
// writable cgroup v2 subtree with the memory and cpu controllers is present, so
// an operator knows before enqueuing a capped runner whether the ceiling would
// actually be enforced or the job refused. Nothing here fails the check — the
// feature is opt-in and off by default; the block only says whether turning it
// on would work. It reports the daemon's own environment, so it is meaningful
// only on the box the daemon runs on (empty on a bare client workstation).
func reportCaps(w io.Writer) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("per-job caps:\n")

	// Empty root uses the real cgroup mount; ProbeCaps changes nothing.
	res := executor.ProbeCaps("")
	if res.Available {
		p("  %-18s available — memory/cpu caps enforce via a delegated cgroup v2 subtree\n", "cgroup v2")
		p("  %-18s %s\n", "subtree", res.Base)
	} else {
		p("  %-18s unavailable — %s\n", "cgroup v2", res.Detail)
		p("  %-18s a capped runner would be REFUSED (fail-closed), never run uncapped\n", "effect")
	}
}

// sandboxOptions is the grant set every kind starts from. The data dir is named
// so the profile guard can refuse a grant that would cover the database or the
// keys, and the vendored collections are granted because a job that cannot read
// them cannot run offline, which is why they were vendored.
func sandboxOptions(dataDir string) executor.SandboxOptions {
	read := []string{filepath.Join(dataDir, collectionsCacheDir)}
	for _, t := range optionalTools {
		read = append(read, executor.ToolGrants(t.name)...)
	}
	read = append(read, executor.PythonInstallGrants(dataDir)...)
	return executor.SandboxOptions{DataDir: dataDir, ReadPaths: read}
}

// jobCaps is the daemon-lifetime cgroup controller a capped job's
// SandboxOptions is built against. It is prepared lazily, on the first job
// that actually names a runner with caps set — most fleets never do, and
// PrepareCgroupController vacates the daemon's own cgroup into a child leaf
// (docs/design/byo-runner.md §6), a real, one-way change to the daemon's own
// cgroup layout that a capless fleet should never pay for just by existing.
// Once resolved (successfully or not) the same controller, or the same
// failure, is reused for every later job: the subtree is a daemon-lifetime
// resource, not a per-job one.
var jobCaps struct {
	once sync.Once
	// root overrides the cgroup mount for tests; empty uses the real
	// cgroupRoot PrepareCgroupController defaults to. Production code never
	// sets this.
	root string
	ctrl *executor.CgroupController
	err  error
}

// capsController returns the daemon's delegated cgroup controller, preparing
// it on first use. A non-nil error means per-job caps cannot be enforced on
// this host; the caller (withCaps, through applyJobCaps) refuses the capped
// job rather than running it uncapped — Decision 0005's "never silently" posture,
// extended from the sandbox tier to the caps feature. warn receives one line
// the first time preparation is attempted, successful or not, so the
// daemon's own log carries the same signal `exocomp doctor` reports under
// "per-job caps" (reportCaps).
func capsController(warn io.Writer) (*executor.CgroupController, error) {
	jobCaps.once.Do(func() {
		jobCaps.ctrl, jobCaps.err = executor.PrepareCgroupController(jobCaps.root)
		if warn == nil {
			return
		}
		if jobCaps.err != nil {
			_, _ = fmt.Fprintf(warn, "WARNING: per-job caps requested but unavailable: %v; "+
				"a job naming a runner with caps set is refused, never run uncapped\n", jobCaps.err)
		} else {
			_, _ = fmt.Fprintln(warn, "per-job caps: delegated cgroup v2 subtree prepared")
		}
	})
	return jobCaps.ctrl, jobCaps.err
}

// resolveRunnerCaps looks up the caps a job's runner reference names, so the
// execution path can carry them into SandboxOptions.Caps. An empty name is
// the default, uncapped path most jobs take. A name that does not resolve to
// a registered runner, or whose stored memory_max/cpu_quota no longer parse,
// refuses the job outright: an operator who named a runner asked for its
// caps, and silently dropping that ask because the row is gone or malformed
// is exactly the "never silently" failure this feature exists to prevent.
func resolveRunnerCaps(ctx context.Context, st *store.Store, runnerName string) (executor.Caps, error) {
	if runnerName == "" {
		return executor.Caps{}, nil
	}
	r, err := st.RunnerByName(ctx, runnerName)
	if err != nil {
		return executor.Caps{}, fmt.Errorf("runner %q: %w", runnerName, err)
	}
	caps, err := executor.ParseCaps(r.MemoryMax, r.CPUQuota)
	if err != nil {
		return executor.Caps{}, fmt.Errorf("runner %q: %w", runnerName, err)
	}
	return caps, nil
}

// requireSandboxedForCaps refuses a capped job outright when the effective
// tier is not enforcing. applyJobCaps (internal/executor/sandbox_drivers.go)
// only runs inside the sandboxed drivers — a plain executor.NewAnsible/NewTofu
// subprocess never looks at SandboxOptions.Caps at all — so a job with
// non-empty caps that fell back to the bare subprocess boundary would
// otherwise run fully uncapped with no refusal: a second "never silently" gap
// alongside the cgroup-availability one applyJobCaps itself already closes.
func requireSandboxedForCaps(ctx context.Context, caps executor.Caps) error {
	if caps.Empty() || sandboxed(ctx) {
		return nil
	}
	return fmt.Errorf("caps requested but the %s tier is not active on this host; "+
		"refusing to run the job uncapped (see `exocomp doctor`'s sandbox block)",
		executor.TierEnforcing)
}

// withCaps augments sb with a job's resolved caps, preparing the daemon's
// cgroup controller on first use. Empty caps are a no-op — most jobs carry
// none, so nothing about the request or the daemon's cgroup layout changes
// for them. Non-empty caps with no usable controller leave sb.Cgroup nil, so
// applyJobCaps refuses the job rather than running it uncapped.
func withCaps(sb executor.SandboxOptions, caps executor.Caps, warn io.Writer) executor.SandboxOptions {
	if caps.Empty() {
		return sb
	}
	sb.Caps = caps
	sb.Cgroup, _ = capsController(warn)
	return sb
}

// ansibleTierDriver returns the ansible driver for the effective tier, with
// caps applied when non-empty. Under the sandbox the private directory moves
// inside the 0700 data dir: the vault password file is written there, and the
// system temp dir is somewhere every other job on the host can also name.
func ansibleTierDriver(dataDir string, opts executor.AnsibleOptions, caps executor.Caps) (executor.Driver, error) {
	if !sandboxed(context.Background()) {
		if err := requireSandboxedForCaps(context.Background(), caps); err != nil {
			return nil, err
		}
		return executor.NewAnsible(opts), nil
	}
	if opts.PrivateDirRoot == "" {
		root := filepath.Join(dataDir, privateRunDir)
		if err := os.MkdirAll(root, 0o700); err == nil {
			opts.PrivateDirRoot = root
		}
	}
	return executor.NewSandboxedAnsible(opts, withCaps(sandboxOptions(dataDir), caps, os.Stderr)), nil
}

// tofuTierDriver returns the OpenTofu driver for the effective tier, with caps
// applied when non-empty.
func tofuTierDriver(dataDir string, opts executor.TofuOptions, caps executor.Caps) (executor.Driver, error) {
	if !sandboxed(context.Background()) {
		if err := requireSandboxedForCaps(context.Background(), caps); err != nil {
			return nil, err
		}
		return executor.NewTofu(opts), nil
	}
	return executor.NewSandboxedTofu(opts, withCaps(sandboxOptions(dataDir), caps, os.Stderr)), nil
}

// reportSandbox writes the doctor's sandbox block: the launcher's version
// against the floor Decision 0005 pins, the kernel's Landlock ABI, the tier in
// effect, and what the probe actually asserted.
func reportSandbox(ctx context.Context, w io.Writer) {
	res := sandboxTier(ctx)
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

	p("sandbox:\n")
	if res.NonoVersion == "" {
		p("  %-18s MISSING (floor %s)\n", executor.NonoBinary, executor.NonoMinVersion)
	} else {
		p("  %-18s %s (floor %s)\n", executor.NonoBinary, res.NonoVersion, executor.NonoMinVersion)
	}

	switch {
	case res.LandlockABI == 0:
		p("  %-18s unavailable\n", "landlock abi")
	case res.LandlockABI < executor.LandlockNetABI:
		p("  %-18s v%d — below v%d, so egress ports fall back to seccomp\n",
			"landlock abi", res.LandlockABI, executor.LandlockNetABI)
	default:
		p("  %-18s v%d\n", "landlock abi", res.LandlockABI)
	}

	p("  %-18s %s (requested %s)\n", "tier", res.Effective, res.Requested)
	switch {
	case res.Enforced:
		p("  %-18s enforced — key read, abstract socket, and ungranted TCP all refused\n", "probe")
	case res.Requested == executor.TierDisabled:
		p("  %-18s not run — %s tier requested\n", "probe", executor.TierDisabled)
	default:
		p("  %-18s DEGRADED to %s: %s\n", "probe", executor.TierDisabled, res.Reason)
	}
	if want := os.Getenv(executor.PythonInstallEnv); want != "" {
		dd, _ := dataDir()
		if grants := executor.PythonInstallGrants(dd); len(grants) > 0 {
			p("  %-18s %s (read-only)\n", "managed python", grants[0])
		} else {
			p("  %-18s %s NOT granted: relative, missing, or overlapping the data dir\n", "managed python", want)
		}
	}
	if res.Enforced {
		if res.ProxyEnforced {
			p("  %-18s enforced — the dependency resolvers' mode, through the filtering proxy\n", "proxy mode")
		} else {
			p("  %-18s NOT enforced, so the dependency resolvers will not run: %s\n", "proxy mode", res.ProxyReason)
		}
	}
	if res.Enforced && !res.PathnameSocketMediated {
		p("  %-18s pathname AF_UNIX connect is not mediated by any Landlock ABI;\n",
			"unix sockets")
		p("  %-18s exocomp therefore places no socket where a job can name one\n", "")
	}
}
