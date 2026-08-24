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
// local | local-sandboxed | auto. Auto is the default and means "sandbox where
// the kernel allows it, and say so loudly where it does not" (ADR 0005).
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
// is written to the warning stream — ADR 0005's "never silently" applies to the
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
				_, _ = fmt.Fprintf(warn, "WARNING: %v; using %s\n", err, executor.TierAuto)
				parsed = executor.TierAuto
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
				executor.TierLocal, executor.TierLocalSandboxed, sandbox.result.Reason)
		}
	})
	return sandbox.result
}

// sandboxed reports whether jobs run under the kernel sandbox.
func sandboxed(ctx context.Context) bool {
	return sandboxTier(ctx).Effective == executor.TierLocalSandboxed
}

// enforceSandboxTier fails closed when the sandbox was asked for by name and
// the host cannot provide it. TierAuto degrades to the subprocess boundary with
// a warning — that is what "auto" means. An explicit local-sandboxed request is
// a policy, not a preference: running the fleet's jobs unsandboxed is not what
// was asked for, so the daemon and the doctor refuse rather than quietly
// downgrade (ADR 0005, "never silently").
func enforceSandboxTier(ctx context.Context) error {
	res := sandboxTier(ctx)
	if res.Requested == executor.TierLocalSandboxed && res.Degraded() {
		return fmt.Errorf("sandbox: %s was requested but cannot be enforced here: %s; "+
			"pass --sandbox %s to accept the subprocess boundary instead",
			executor.TierLocalSandboxed, res.Reason, executor.TierAuto)
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
	printRunSecretFileStaging(w, dataDir, configuredSecretStagingBackend(dataDir))
}

// configuredSecretStagingBackend reads the --secret-staging choice a daemon
// persisted (store.SettingSecretStaging) back from the store, defaulting to
// auto — the same default serve itself uses — when the data dir has no store
// yet or the setting was never written. It stats the database file before
// opening a store, so doctor never creates the data dir as a side effect of
// reporting on it (TestDoctorDoesNotCreateTheDataDir), the same guard
// internal/health.ProbeLitestream uses. Never fails doctor: a resolution
// problem is reported by printRunSecretFileStaging instead.
func configuredSecretStagingBackend(dataDir string) executor.StagingBackend {
	if _, err := os.Stat(filepath.Join(dataDir, "exocomp.db")); err != nil {
		return executor.StagingAuto
	}
	st, err := openStore()
	if err != nil {
		return executor.StagingAuto
	}
	defer func() { _ = st.Close() }()
	raw, err := st.GetSetting(context.Background(), store.SettingSecretStaging)
	if err != nil {
		return executor.StagingAuto
	}
	backend, err := executor.ParseStagingBackend(raw)
	if err != nil {
		return executor.StagingAuto
	}
	return backend
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
	return executor.SandboxOptions{DataDir: dataDir, ReadPaths: read}
}

// ansibleTierDriver returns the ansible driver for the effective tier. Under
// the sandbox the private directory moves inside the 0700 data dir: the vault
// password file is written there, and the system temp dir is somewhere every
// other job on the host can also name.
func ansibleTierDriver(dataDir string, opts executor.AnsibleOptions) executor.Driver {
	if !sandboxed(context.Background()) {
		return executor.NewAnsible(opts)
	}
	if opts.PrivateDirRoot == "" {
		root := filepath.Join(dataDir, privateRunDir)
		if err := os.MkdirAll(root, 0o700); err == nil {
			opts.PrivateDirRoot = root
		}
	}
	return executor.NewSandboxedAnsible(opts, sandboxOptions(dataDir))
}

// tofuTierDriver returns the OpenTofu driver for the effective tier.
func tofuTierDriver(dataDir string, opts executor.TofuOptions) executor.Driver {
	if !sandboxed(context.Background()) {
		return executor.NewTofu(opts)
	}
	return executor.NewSandboxedTofu(opts, sandboxOptions(dataDir))
}

// reportSandbox writes the doctor's sandbox block: the launcher's version
// against the floor ADR 0005 pins, the kernel's Landlock ABI, the tier in
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
	case res.Requested == executor.TierLocal:
		p("  %-18s not run — %s tier requested\n", "probe", executor.TierLocal)
	default:
		p("  %-18s DEGRADED to %s: %s\n", "probe", executor.TierLocal, res.Reason)
	}
	if res.Enforced && !res.PathnameSocketMediated {
		p("  %-18s pathname AF_UNIX connect is not mediated by any Landlock ABI;\n",
			"unix sockets")
		p("  %-18s exocomp therefore places no socket where a job can name one\n", "")
	}
}
