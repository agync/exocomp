package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/deadman"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/hooks"
	"exocomp.dev/exocomp/internal/metrics"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/verify"
	"exocomp.dev/exocomp/internal/web"
)

func init() {
	registerCommand(newServeCmd)
}

// shutdownGrace bounds how long a stopping server waits for the request it is
// already serving. A state push is one INSERT; anything slower is stuck.
const shutdownGrace = 5 * time.Second

// serveWorkers is how many jobs the daemon runs at once, and servePoll how
// often it looks for work another process enqueued: its own enqueues wake it
// immediately, a CLI's do not.
const (
	serveWorkers = 4
	servePoll    = 200 * time.Millisecond
)

// drainPoll is how often shutdown re-reads the job table while it waits for
// the jobs this process still owns.
const drainPoll = 100 * time.Millisecond

// defaultDrainGrace is how long in-flight jobs get to finish once a signal
// arrives. It is generous on purpose: a backup leg writes its own backup_run
// row at the end, and cancelling it mid-write is what leaves the database
// claiming a run that nothing is running.
const defaultDrainGrace = 30 * time.Second

// defaultReloadInterval is how often the daemon rebuilds its schedules from
// the database, so `schedule enable` reaches it without a signal.
const defaultReloadInterval = time.Minute

// restartDrainTimeout bounds how long a restart waits for in-flight jobs to
// reach zero. Past it the restart aborts and lifts the cordon (fail-safe: it
// never cancels work implicitly); --force cancels the remaining jobs and
// restarts instead. It matches the CLI's default --timeout.
const restartDrainTimeout = 10 * time.Minute

// restartPollInterval is how often the restart goroutine re-reads the in-flight
// count while draining.
const restartPollInterval = 250 * time.Millisecond

// restartExec replaces this process with the on-disk binary, so a restart runs
// the new image in place — same pid, no systemd restart, no supervisor in the
// loop. It is a variable so the serve loop's restart path is unit-testable with
// the exec stubbed; on success syscall.Exec never returns.
var restartExec = func() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(self, os.Args, os.Environ())
}

// restartStagingCleanup removes the run-secret staging session directory before
// a restart re-execs. It is a variable so the decision is unit-testable. The
// normal cleanup is runServe's defer, but syscall.Exec never returns, so that
// defer does not run on the exec path — and because exec keeps the same pid,
// the next start's liveness sweep would see the directory's owning pid still
// alive and skip it. Closing it explicitly before the exec is what leaves no
// secret-bearing directory behind.
var restartStagingCleanup = executor.CloseRunSecretStaging

// defaultSweepInterval is how often the daemon fails expired approvals
// closed, re-publishes the ones halfway through their window, and turns a
// plan that found changes into a drift event.
const defaultSweepInterval = 30 * time.Second

// deadmanInterval is how often the daemon re-reads the dead-man's ONE age rule.
// It is the same cadence as the reactive sweep: a trip is paged within half a
// minute of crossing the threshold and a succeeded backup's heartbeat ping
// lands within the same window.
const deadmanInterval = 30 * time.Second

// defaultLeaseTTL is how long the cross-host active lease stays live without a
// renew, and defaultLeaseRenew how often the active daemon refreshes it. The
// renew is a third of the TTL so two renews can be lost — a GC pause, a slow
// replicated write — before a still-active daemon's lease is judged stale by a
// standby. The TTL bounds how long a standby waits after an ungraceful primary
// loss (a clean stop releases at once); a few tens of seconds trades a small
// failover delay for tolerance of ordinary jitter.
const (
	defaultLeaseTTL   = 30 * time.Second
	defaultLeaseRenew = 10 * time.Second
)

// serveOptions is what the daemon is started with.
type serveOptions struct {
	port           int
	drainGrace     time.Duration
	reloadInterval time.Duration
	sweepInterval  time.Duration
	dedupWindow    time.Duration
	approvalTTL    time.Duration
	leaseTTL       time.Duration
	leaseRenew     time.Duration
	forceLease     bool
	hookAllow      string
	publicURL      string
	basePath       string
	sandbox        string
	resolverHosts  []string
	identityAllow  []string
	webAddr        string
	webInsecure    bool
	secretStaging  string
	checkCanary    string
}

func defaultServeOptions() serveOptions {
	return serveOptions{
		drainGrace:     defaultDrainGrace,
		reloadInterval: defaultReloadInterval,
		sweepInterval:  defaultSweepInterval,
		dedupWindow:    reactive.DefaultDedupWindow,
		approvalTTL:    reactive.DefaultApprovalTTL,
		leaseTTL:       defaultLeaseTTL,
		leaseRenew:     defaultLeaseRenew,
		secretStaging:  string(executor.StagingAuto),
	}
}

func newServeCmd() *cobra.Command {
	opts := defaultServeOptions()

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the exocompd daemon",
		Long: "One process: the OpenTofu HTTP state backend on 127.0.0.1, the job " +
			"queue's workers, and the scheduler. Jobs left behind by a process that " +
			"is gone are swept at startup. SIGHUP rebuilds the schedules from the " +
			"database; SIGINT and SIGTERM stop new work and let what is running " +
			"finish before the backend it pushes state to goes away.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}
	cmd.Flags().IntVar(&opts.port, "port", 0, "loopback port to bind (0: ephemeral)")
	cmd.Flags().DurationVar(&opts.drainGrace, "drain-grace", opts.drainGrace,
		"how long in-flight jobs may finish after a signal before they are cancelled")
	cmd.Flags().DurationVar(&opts.reloadInterval, "reload-interval", opts.reloadInterval,
		"how often schedules are rebuilt from the database (0: only on SIGHUP)")
	cmd.Flags().DurationVar(&opts.sweepInterval, "sweep-interval", opts.sweepInterval,
		"how often approvals are expired and re-published and drift is collected")
	cmd.Flags().DurationVar(&opts.dedupWindow, "dedup-window", opts.dedupWindow,
		"how long a repeat of the same condition is a duplicate")
	cmd.Flags().DurationVar(&opts.approvalTTL, "approval-ttl", opts.approvalTTL,
		"how long a decision link is good for before the job is rejected")
	cmd.Flags().StringVar(&opts.hookAllow, "hook-allow", "",
		"comma-separated CIDRs webhooks may come from (empty: any)")
	cmd.Flags().StringVar(&opts.publicURL, "public-url", "",
		"base URL decision links point at (empty: the bound loopback address)")
	cmd.Flags().StringVar(&opts.basePath, "base-path", "",
		"origin sub-path the web surface is served under (e.g. /exocomp); empty serves at the origin root")
	cmd.Flags().StringVar(&opts.webAddr, "web-addr", "",
		"also serve the token-gated LAN web+API surface here over TLS (Decision 0009); empty starts no web listener")
	cmd.Flags().BoolVar(&opts.webInsecure, "web-insecure-http", false,
		"serve the web surface as cleartext HTTP (loud, never the documented path)")
	cmd.Flags().StringSliceVar(&opts.resolverHosts, "resolver-allow-domain", nil,
		"extra package index host the dependency resolvers may reach through the sandbox proxy (repeatable), "+
			"beyond pypi.org, files.pythonhosted.org and the Ansible Galaxy hosts")
	cmd.Flags().StringArrayVar(&opts.identityAllow, "allow-ssh-identity", nil,
		"an ssh identity key file a playbook run enqueued through /api/v1 may ask to read (repeatable), "+
			"one regular file outside the data dir, never a directory")
	cmd.Flags().StringVar(&opts.sandbox, "sandbox", "",
		"job isolation tier: opportunistic, enforcing, or disabled (default: $"+sandboxTierEnv+", else enforcing)")
	cmd.Flags().DurationVar(&opts.leaseTTL, "lease-ttl", opts.leaseTTL,
		"how long the cross-host active lease stays live without a renew")
	cmd.Flags().DurationVar(&opts.leaseRenew, "lease-renew", opts.leaseRenew,
		"how often the active daemon renews the cross-host lease (keep well below --lease-ttl)")
	cmd.Flags().BoolVar(&opts.forceLease, "force-lease", false,
		"take over an active lease still held by another daemon; use only when that holder is truly gone")
	cmd.Flags().StringVar(&opts.secretStaging, "secret-staging", opts.secretStaging,
		"where a run-secret staged as a file lands: auto, tmpfs, or disk (auto falls back to disk, loudly, when tmpfs is unusable)")
	cmd.Flags().StringVar(&opts.checkCanary, "check-canary", "",
		"ip:port dialed before a check sweep declares targets down (empty: disabled) — a lost uplink would otherwise read as mass-down")
	return cmd
}

// recordImageRef persists the deployment image ref read from EXOCOMP_IMAGE at
// startup. An empty value is not a change: the store keeps whatever a previous
// start recorded, so a restart without the env does not erase what RECOVERY.md
// names.
func recordImageRef(ctx context.Context, st *store.Store, image string) error {
	if image == "" {
		return nil
	}
	return st.PutSetting(ctx, store.SettingImageRef, image)
}

// validateCheckCanary refuses a non-empty --check-canary that is not an ip:port
// at startup. A malformed value is a canary that can never dial, so every sweep
// with a down target reads as a missed observation and no transition ever
// emits — exactly the silent mass-down suppression the flag exists to prevent,
// inverted. Empty (disabled) is valid.
func validateCheckCanary(canary string) error {
	if canary == "" {
		return nil
	}
	ap, err := netip.ParseAddrPort(canary)
	if err != nil {
		return fmt.Errorf("--check-canary %q must be ip:port (e.g. 192.168.1.1:443): %w", canary, err)
	}
	if ap.Port() == 0 {
		return fmt.Errorf("--check-canary %q: port must be 1..65535", canary)
	}
	return nil
}

func runServe(ctx context.Context, out, warn io.Writer, opts serveOptions) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("serve: resolve data dir: %w", err)
	}

	if err := validateCheckCanary(opts.checkCanary); err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	if err := requestSandboxTier(opts.sandbox, warn); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	if err := configureResolverHosts(opts.resolverHosts); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	identityAllow, err := executor.NormalizeIdentityAllowlist(dir, opts.identityAllow)
	if err != nil {
		return fmt.Errorf("serve: --allow-ssh-identity: %w", err)
	}
	if err := enforceSandboxTier(ctx); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	_, _ = fmt.Fprintf(out, "job isolation tier: %s\n", sandboxTier(ctx).Effective)

	lock, err := acquireServeLock(dir)
	if err != nil {
		return err
	}
	defer lock.release()

	// secretStagingDiskRoot is the disk staging root regardless of which
	// backend ends up resolved: the same "<data-dir>/run/" tree job private
	// dirs already use (jobRunDir), reused rather than reinvented, and
	// excluded from backup by the same construction that already excludes
	// every other job-scratch path under it (docs/log.md).
	secretStagingDiskRoot := filepath.Join(dir, privateRunDir)

	// Defense-in-depth for the run-secret file staging path, third of the
	// three cleanup layers (per-job unlink; whole-session RemoveAll on a
	// graceful stop, below; this liveness-checked sweep for everything a
	// broken cleanup path left behind — a panic between staging and Close, a
	// kill -9). Both possible roots are swept unconditionally, independent of
	// which backend --secret-staging resolves to this run: an operator who
	// switched backends since the last start can still have orphans on the
	// one no longer in use. A directory whose owning pid is still alive is
	// never touched, so this can never delete a concurrent sibling instance's
	// in-flight secrets. Runs once per start, after the single-instance lock
	// so two daemons never sweep concurrently, and before the store opens —
	// it needs no setting, only the two well-known roots.
	if swept, err := executor.SweepOrphanRunSecretFiles(executor.TmpfsRoot, secretStagingDiskRoot); err != nil {
		_, _ = fmt.Fprintf(warn, "WARNING: startup sweep of orphan run-secret staging dirs: %v\n", err)
	} else if swept > 0 {
		_, _ = fmt.Fprintf(out, "swept %d orphaned run-secret staging dir(s)\n", swept)
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	secretStagingBackend, err := executor.ParseStagingBackend(opts.secretStaging)
	if err != nil {
		return fmt.Errorf("serve: --secret-staging: %w", err)
	}
	staging, err := executor.ConfigureRunSecretStaging(executor.StagingConfig{
		Backend:  secretStagingBackend,
		DiskRoot: secretStagingDiskRoot,
		Warn:     warn,
	})
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	// Removes this instance's whole staging session directory — the
	// graceful-shutdown cleanup layer — once every other deferred shutdown
	// step above it (jobs drained, queue stopped) has already run its own
	// deferred cleanup, so nothing still using a staged file races the
	// RemoveAll. A crash skips this defer entirely, which is exactly what the
	// startup liveness sweep exists to reap on the next start.
	defer func() { _ = executor.CloseRunSecretStaging() }()
	if err := st.PutSetting(ctx, store.SettingSecretStaging, string(secretStagingBackend)); err != nil {
		return fmt.Errorf("serve: persist --secret-staging: %w", err)
	}
	_, _ = fmt.Fprintf(out, "run-secret file staging: %s backend at %s\n", staging.Backend, staging.Root)

	// The deployment's image ref, set by the unit's EXOCOMP_IMAGE, is recorded
	// so RECOVERY.md and `exocomp doctor` name what a recovered host must load
	// (Decision 0016). A restart with a changed image updates it; an unset env
	// leaves the stored value alone.
	if err := recordImageRef(ctx, st, os.Getenv("EXOCOMP_IMAGE")); err != nil {
		return fmt.Errorf("serve: persist EXOCOMP_IMAGE: %w", err)
	}

	// The cross-host fence is taken BEFORE the boot sweep, not after. The sweep's
	// liveness test is host-local pid probing, so a standby that swept a live
	// primary's replicated store would retire the primary's in-flight jobs as if
	// their owner were dead. Refusing here — or taking a stale/forced lease —
	// keeps a fenced-out daemon from touching another daemon's rows at all. The
	// host-local flock above still guards a same-host double start.
	fence, err := acquireLease(ctx, st, leaseHolderID(), opts.leaseTTL, opts.leaseRenew, opts.forceLease, out)
	if err != nil {
		return err
	}
	defer fence.release(context.WithoutCancel(ctx), warn)

	if err := bootSweepJobs(ctx, st, warn); err != nil {
		return err
	}

	if err := flagStaleLocks(ctx, st, warn); err != nil {
		return err
	}

	// Finish a DEK rotation an earlier crash left half-applied before opening the
	// envelope: until it completes, keyring.age wraps the old DEK while some rows
	// already moved to the new one, so those rows would fail to open. Reconcile is
	// a no-op when nothing is staged.
	if reconciled, err := secrets.Reconcile(dir, storeReseal(ctx, st)); err != nil {
		return fmt.Errorf("serve: finish interrupted DEK rotation: %w", err)
	} else if reconciled {
		_, _ = fmt.Fprintln(out, "recovered an interrupted DEK rotation: every secret is now under the new key")
	}

	seal, err := secrets.SealConfigFromEnv()
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	if seal.Configured() {
		_, _ = fmt.Fprintf(out, "envelope seal: openbao transit key %q at %s\n", seal.KeyName, seal.Address)
	}
	env, err := secrets.OpenWithSeal(dir, seal)
	if err != nil {
		return fmt.Errorf("serve: open envelope (run `exocomp init` first): %w", err)
	}
	defer func() { _ = env.Close() }()

	srv, err := api.New(api.Options{Store: st, Tokens: env, Port: opts.port})
	if err != nil {
		return err
	}

	// Jobs run under a context of their own, not the signal context: a
	// SIGTERM has to stop new work without cancelling a leg that is halfway
	// through writing the rows that say how it ended. Shutdown cancels this
	// one itself, and only once the grace window is spent.
	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()
	finalCtx := context.WithoutCancel(ctx)

	drv, err := serveDriver(dir, st, env, srv)
	if err != nil {
		return err
	}
	// The drain controller is built before the queue so the queue's claim gate
	// can consult it; its scheduler handle is filled in once the scheduler
	// exists (below). Until then it is uncordoned, so the gate is open. It also
	// owns the restart drain: cancelJobs is the --force lever that cancels the
	// jobs a drain is still waiting on.
	cordon := newCordonController(st, cancelJobs)
	q := queue.New(st, drv, queue.Options{Workers: serveWorkers, PollInterval: servePoll},
		queue.WithRedactorProvider(func() *secrets.Redactor {
			return secrets.NewRedactor(knownSecrets(jobCtx, st, env, warn))
		}),
		queue.WithClaimGate(cordon.claimAllowed),
		queue.WithoutDrainSet())

	// The reactive surfaces are mounted on the one listener before it binds:
	// routes are fixed at start, and a second listener would be a second
	// authentication story.
	engine, dm, notifier, sealed, err := reactiveEngine(ctx, st, q, env, warn, dir, opts)
	if err != nil {
		return err
	}
	allow, err := reactive.ParseAllow(opts.hookAllow)
	if err != nil {
		return fmt.Errorf("serve: --hook-allow: %w", err)
	}
	ingress, err := reactive.NewIngress(reactive.IngressOptions{
		Engine:  engine,
		Deriver: env,
		Allow:   allow,
	})
	if err != nil {
		return err
	}
	if err := srv.Mount(ingress.Mounts()); err != nil {
		return err
	}

	// Self-observability rides the same listener: the staleness gauges read
	// the store the daemon already owns, so a second endpoint would only be a
	// second bind and a second auth story to get wrong. The listener is
	// loopback but reachable by tofu-tier jobs, so the scrape is bearer-gated
	// with a token derived from the envelope. No command prints it, so
	// docs/operations/notifications.md derives it by hand.
	if err := srv.Mount(map[string]http.Handler{
		"GET /metrics": metrics.Authenticated(metrics.New(st, dir).Handler(), env),
	}); err != nil {
		return err
	}

	if err := srv.Start(); err != nil {
		return err
	}
	if err := api.WriteAddress(filepath.Join(dir, api.AddressFile), srv.Addr()); err != nil {
		return err
	}
	// Persisted so a separate `approval link` invocation can read the same
	// base back (internal/reactive.Engine.SetBaseURL below only holds it in
	// this process's memory).
	if opts.publicURL != "" {
		if err := st.PutSetting(jobCtx, store.SettingPublicURL, opts.publicURL); err != nil {
			return err
		}
	}
	base := opts.publicURL
	if base == "" {
		base = "http://" + srv.Addr()
	}
	engine.SetBaseURL(base)
	_, _ = fmt.Fprintf(out, "state backend listening on http://%s\n", srv.Addr())
	printHooks(out, env, base)

	// The LAN web/token surface is a second, separate listener: non-loopback
	// and TLS, token-gated per Decision 0009, sharing this process's engine so an
	// approval decided in a browser releases the job into this queue. The
	// loopback state backend above stays loopback-only and takes no LAN token.
	var surface *webSurface
	if opts.webAddr != "" {
		surface, err = startWebSurface(ctx, st, env, dir, engine, cordon, newNtfySeam(st, env), opts.basePath, opts.webAddr, opts.webInsecure, identityAllow, warn)
		if err != nil {
			shutdownCtx, cancel := context.WithTimeout(finalCtx, shutdownGrace)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
			return err
		}
		_, _ = fmt.Fprintf(out, "web surface listening on %s://%s\n", surface.scheme, surface.addr)
		if surface.fingerprint != "" {
			_, _ = fmt.Fprintf(out, "certificate SHA-256: %s\n", surface.fingerprint)
		}
	}

	qDone := make(chan error, 1)
	go func() { qDone <- q.Run(jobCtx) }()
	_, _ = fmt.Fprintf(out, "queue draining with %d workers\n", serveWorkers)

	s := sched.New(st, q, nil, sched.WithSourceResolver(service.New(st, dir, nil).ResolvePlainSourcePlay))
	if err := s.Start(jobCtx); err != nil {
		cancelJobs()
		if qErr := <-qDone; qErr != nil {
			err = errors.Join(err, qErr)
		}
		return fmt.Errorf("serve: %w", err)
	}
	cordon.setScheduler(s)
	// The notify transports are the other read-once set a reload can refresh:
	// an operator who sets, changes, or clears the ntfy configuration (or seals
	// a goalert key, or flips the ntfy Detail setting) reaches the running
	// daemon through the same reload seam SIGHUP and `exocomp reload` drive,
	// with no restart. The silent-phone warning is emitted only when a reload
	// *clears* the configured set: the boot
	// warning already covered "never configured", and a reload-interval tick
	// must not repeat it every minute.
	var lastSealed atomic.Int32
	lastSealed.Store(int32(sealed))
	cordon.setReloadExtra(func(ctx context.Context) error {
		transports, nowSealed, err := loadNotifyTransports(ctx, st, sealedLookup(st, env), ntfyPublicURL(ctx, st, ""))
		if err != nil {
			return err
		}
		notifier.SetTransports(transports...)
		if nowSealed == 0 && lastSealed.Load() != 0 {
			warnNoPushTransport(warn, nowSealed)
		}
		lastSealed.Store(int32(nowSealed))
		return nil
	})
	_, _ = fmt.Fprintln(out, "scheduler started")

	// The lease keeps this daemon's claim to be the active one alive. Losing it
	// to a force-takeover stands the daemon down through the ordinary signal path
	// (stop), so a resurrected primary that finds itself fenced out stops rather
	// than racing the daemon that took over.
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		fence.renewUntil(jobCtx, stop, warn)
	}()

	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		sweepReactive(jobCtx, st, engine, warn, opts.sweepInterval)
	}()

	// The dead-man trip monitor runs beside the reactive sweep: it compares the
	// ONE backup-age rule to the operator's threshold, pages the fan-out once
	// when it trips, and pings the sealed heartbeat URL on each new succeeded
	// backup. It shares this process's notifier (built in reactiveEngine), so
	// the push path is the daemon's, not the web surface's.
	dmDone := make(chan struct{})
	go func() {
		defer close(dmDone)
		dm.Run(jobCtx, deadmanInterval)
	}()

	// The step-ledger sequencer runs here, after the boot job-sweep (openJobStore
	// retired interrupted rows) and after the queue's workers are draining: its
	// first pass is ResumeAll, so a workflow a crash left mid-flight continues
	// from the ledger, and it then keeps live workflows — the apply->play
	// handoff — progressing. The queue stays the sole executor; this only
	// releases each next step and records its outcome (Decision 0008).
	seq := queue.NewSequencer(q)
	seqDone := make(chan struct{})
	go func() {
		defer close(seqDone)
		driveWorkflows(jobCtx, seq, out, warn)
	}()

	var webErr <-chan error
	if surface != nil {
		webErr = surface.serveErr
	}
	restarting, webFailure := serveUntilSignal(sigCtx, jobCtx, out, warn, hup, cordon.reloadAll, opts.reloadInterval, webErr, cordon.restartReady)
	if webFailure != nil {
		_, _ = fmt.Fprintf(warn, "WARNING: web surface failed: %v\n", webFailure)
	}
	_, _ = fmt.Fprintln(out, "shutting down")

	err = shutdownServe(finalCtx, out, warn, shutdown{
		store:      st,
		sched:      s,
		server:     srv,
		web:        surface,
		cancelJobs: cancelJobs,
		queueDone:  qDone,
		grace:      opts.drainGrace,
	})
	<-sweepDone
	<-seqDone
	<-leaseDone
	<-dmDone
	if err == nil {
		err = webFailure
	}
	if !restarting {
		return err
	}
	return finishRestart(out, err)
}

// finishRestart runs the restart's last step: re-exec the on-disk binary once
// the shutdown completed cleanly. A shutdown that errored leaves the daemon in
// an unknown state, so it aborts rather than exec'ing into an unclean process
// (the caller's err is returned and the process exits on it). The exec itself is
// injectable (restartExec) so this decision is unit-testable without a real
// exec.
func finishRestart(out io.Writer, shutdownErr error) error {
	if shutdownErr != nil {
		_, _ = fmt.Fprintf(out, "restart aborted: shutdown did not complete cleanly: %v\n", shutdownErr)
		return shutdownErr
	}
	// The run-secret staging session directory must be gone before the exec:
	// syscall.Exec skips runServe's deferred CloseRunSecretStaging, and the
	// next start's sweep would leave the directory because the pid is unchanged
	// by an in-place exec.
	if err := restartStagingCleanup(); err != nil {
		_, _ = fmt.Fprintf(out, "WARNING: run-secret staging cleanup before restart: %v\n", err)
	}
	_, _ = fmt.Fprintln(out, "restarting: re-exec'ing the on-disk binary")
	if err := restartExec(); err != nil {
		return fmt.Errorf("serve: restart: re-exec on-disk binary: %w", err)
	}
	return nil
}

// bootSweepJobs reconciles every non-terminal job a crash stranded — rows a dead
// process left queued or running, and gated rows whose approval decision was
// interrupted — the same repair openJobStore runs for the CLIs, but serve runs it
// only after the cross-host fence is held, so a fenced-out standby never sweeps a
// live primary's rows. Its pid-liveness test is host-local by design (invariant 1:
// one host owns the store at a time), which the lease now enforces across hosts.
func bootSweepJobs(ctx context.Context, st *store.Store, warn io.Writer) error {
	rec, err := st.ReconcileNonTerminal(ctx)
	if err != nil {
		return err
	}
	if warn != nil {
		if gone := rec.Interrupted + rec.Orphaned; gone > 0 {
			_, _ = fmt.Fprintf(warn, "swept %d job(s) left by a process that is gone to interrupted\n", gone)
		}
		if settled := rec.Released + rec.Cancelled; settled > 0 {
			_, _ = fmt.Fprintf(warn, "reconciled %d job(s) whose approval decision was interrupted\n", settled)
		}
	}
	return nil
}

// sequencerInterval is how often the daemon re-drives incomplete workflows, so
// a chain a CLI just started is picked up promptly without a signal.
const sequencerInterval = time.Second

// driveWorkflows resumes crash-interrupted workflows at boot and keeps live
// ones progressing. Each pass asks the ledger which workflows are unfinished
// and runs their remaining steps; a succeeded step is never re-run, and a
// failed step leaves the rest blocked. It runs until the job context ends.
//
// Workflows are driven one at a time within a pass: at homelab scale a handoff
// runs to completion before the next is picked up, which keeps the ledger the
// single source of truth without a second scheduler.
func driveWorkflows(ctx context.Context, seq *queue.Sequencer, out, warn io.Writer) {
	ticker := time.NewTicker(sequencerInterval)
	defer ticker.Stop()

	boot := true
	for {
		resumed, err := seq.ResumeAll(ctx)
		if err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintf(warn, "WARNING: drive workflows: %v\n", err)
		}
		if boot {
			if resumed > 0 {
				_, _ = fmt.Fprintf(out, "resumed %d workflow(s) from the ledger\n", resumed)
			}
			boot = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sealedLookup reads a sealed secret's plaintext by name. Both the reactive
// transports and the dead-man heartbeat read through it, and a daemon reload
// rebuilds them through the same one. ntfy's server URL and topic are ordinary
// settings now: a setting wins when present, so the value the UI shows is the
// value the transport uses, and a sealed ntfy/url or ntfy/topic is only the
// fallback for an install from before the settings move.
func sealedLookup(st *store.Store, env *secrets.Envelope) notify.SecretLookup {
	return func(ctx context.Context, name string) ([]byte, error) {
		if v, ok := ntfySetting(ctx, st, name); ok {
			return v, nil
		}
		row, err := st.GetSecret(ctx, name)
		if err != nil {
			return nil, err
		}
		return env.OpenSecret(secrets.SealedSecret{
			Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
		})
	}
}

// ntfyPublicURL is the base a notification's deep link is rooted at: the
// operator's --public-url when this run passed one, else the address a previous
// run persisted (store.SettingPublicURL). Empty when neither is set, which
// leaves a notification's link as the raw deep link rather than a loopback
// address no phone can reach.
func ntfyPublicURL(ctx context.Context, st *store.Store, override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if v, err := st.GetSetting(ctx, store.SettingPublicURL); err == nil {
		return strings.TrimSpace(v)
	}
	return ""
}

// ntfySetting resolves ntfy/url and ntfy/topic from the settings table: those
// two are ordinary configuration, so a setting is authoritative and the sealed
// row is only a fallback.
func ntfySetting(ctx context.Context, st *store.Store, name string) ([]byte, bool) {
	var key string
	switch name {
	case notify.SecretNtfyURL:
		key = store.SettingNtfyURL
	case notify.SecretNtfyTopic:
		key = store.SettingNtfyTopic
	default:
		return nil, false
	}
	if v, err := st.GetSetting(ctx, key); err == nil && v != "" {
		return []byte(v), true
	}
	return nil, false
}

// loadNotifyTransports builds the daemon's push fan-out from the configured
// transports. It returns the transports and how many are configured; UnifiedPush
// is store-backed and always wired, so it is never counted. publicURL roots a
// notification's deep link at the deployment's public address. A refused sealed
// configuration is an error — running blind past it would mean approvals and
// failures nobody is told about — but an unconfigured transport is a nil, not an
// error: an operator who configured only one, or neither, still gets a working
// daemon.
func loadNotifyTransports(ctx context.Context, st *store.Store, look notify.SecretLookup, publicURL string) ([]notify.Transport, int, error) {
	var transports []notify.Transport

	ntfyT, err := notify.LoadNtfy(ctx, look)
	if err != nil {
		return nil, 0, fmt.Errorf("serve: %w", err)
	}
	if ntfyT != nil {
		// An unset or unrecognized value stays the documented default
		// (descriptive): GetSetting's error on an absent key is not this
		// function's to report, only to fall back from.
		if raw, err := st.GetSetting(ctx, store.SettingNtfyDetail); err == nil {
			ntfyT.SetDetail(notify.Detail(raw))
		}
		ntfyT.SetPublicURL(publicURL)
		transports = append(transports, ntfyT)
	}

	goalertT, err := notify.LoadGoAlert(ctx, look)
	if err != nil {
		return nil, 0, fmt.Errorf("serve: %w", err)
	}
	if goalertT != nil {
		transports = append(transports, goalertT)
	}

	sealed := len(transports)
	transports = append(transports, notify.NewUnifiedPush(st, slog.Default()))
	return transports, sealed, nil
}

// warnNoPushTransport warns when no push transport is configured. The
// silent-phone warning is about the transports: UnifiedPush needs no secret and
// registers its targets at runtime, so its presence must not mask an operator
// who configured neither ntfy nor goalert. ntfy is configured by its URL and
// topic settings (with an optional sealed token); goalert by its sealed keys.
// It is emitted at boot, and again when a reload clears a set that had been
// configured.
func warnNoPushTransport(warn io.Writer, sealed int) {
	if sealed != 0 {
		return
	}
	_, _ = fmt.Fprintf(warn, "WARNING: no push transport configured "+
		"(ntfy: %s + %s settings, optional sealed %s; goalert: sealed %s + %s); "+
		"approvals and job failures will alert silently\n",
		store.SettingNtfyURL, store.SettingNtfyTopic, notify.SecretNtfyToken,
		notify.SecretGoAlertURL, notify.SecretGoAlertKey)
}

// reactiveEngine builds the rule engine, with push configured from the ntfy
// settings and the sealed keys. An operator who has not configured an ntfy
// endpoint gets a working daemon and a silent phone, not a daemon that refuses
// to start. It also builds
// the dead-man trip monitor over the same notifier, so the daemon-side push
// path shares the one fan-out every other event uses. The notifier and the
// sealed-transport count are returned so a reload can replace its transports in
// place (see loadNotifyTransports).
func reactiveEngine(ctx context.Context, st *store.Store, q *queue.Queue,
	env *secrets.Envelope, warn io.Writer, dir string, opts serveOptions) (*reactive.Engine, *deadman.Monitor, *notify.Notifier, int, error) {
	look := sealedLookup(st, env)

	transports, sealed, err := loadNotifyTransports(ctx, st, look, ntfyPublicURL(ctx, st, opts.publicURL))
	if err != nil {
		return nil, nil, nil, 0, err
	}
	// A notify_mute row holds the push (jobs still run, the outcome is
	// recorded); the checker reads the store per notify, so a mute set while
	// the daemon runs takes effect at once. Without it a mute was inert.
	notifier := notify.New(nil, transports...).WithMutes(notify.NewStoreMutes(st))
	warnNoPushTransport(warn, sealed)

	engine, err := reactive.New(reactive.Options{
		Store:       st,
		Queue:       q,
		Deriver:     env,
		Notifier:    notifier,
		Playbooks:   service.New(st, dir, nil),
		DedupWindow: opts.dedupWindow,
		ApprovalTTL: opts.approvalTTL,
		Canary:      opts.checkCanary,
		Redactor: func() reactive.Redactor {
			return secrets.NewRedactor(knownSecrets(ctx, st, env, warn))
		},
	})
	if err != nil {
		return nil, nil, nil, 0, err
	}

	// The dead-man monitor reads the heartbeat URL plaintext only to ping it;
	// everything it reports comes from the store, so the web never sees the
	// value. Its host is the daemon's, named in the page a mute can hold.
	host := leaseHostname()
	monitor := deadman.NewMonitor(deadman.Options{
		Store: st, Look: look, Notifier: notifier, Host: host, Logger: slog.Default(),
	})
	return engine, monitor, notifier, sealed, nil
}

// printHooks tells the operator where each source posts. The tokens are
// derived, so they are the same after a restore and this is the only place
// they have to be looked up.
func printHooks(out io.Writer, env *secrets.Envelope, base string) {
	for _, source := range reactive.Sources {
		_, _ = fmt.Fprintf(out, "hook %-14s %s\n", source, reactive.HookURL(base, env, source))
	}
}

// sweepReactive runs the clock-driven halves of the reactive layer: every
// non-terminal job a crash stranded is reconciled, approvals whose TTL ran out
// fail closed, approvals halfway through their window get one more push, a
// scheduled plan that found changes becomes a drift event, a job that ended in a
// terminal failure pages the operator once, and a secret past (or nearing) its
// expiry pages once. It runs until the job context is cancelled.
//
// The job reconcile leads: it is what finishes an approval decision the reactive
// half recorded but was interrupted before it could release or withdraw the job,
// and it retires gated rows nothing can ever decide. It is idempotent, so running
// it on the same tick as the approval sweep only ever settles what is stranded.
func sweepReactive(ctx context.Context, st *store.Store, engine *reactive.Engine, warn io.Writer, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if rec, err := st.ReconcileNonTerminal(ctx); err != nil {
			if ctx.Err() == nil {
				_, _ = fmt.Fprintf(warn, "WARNING: reconcile jobs: %v\n", err)
			}
		} else if rec.Total() > 0 {
			_, _ = fmt.Fprintf(warn, "reconciled jobs: %d interrupted, %d orphaned, %d released, %d cancelled\n",
				rec.Interrupted, rec.Orphaned, rec.Released, rec.Cancelled)
		}

		if expired, republished, err := engine.SweepApprovals(ctx); err != nil {
			if ctx.Err() == nil {
				_, _ = fmt.Fprintf(warn, "WARNING: sweep approvals: %v\n", err)
			}
		} else if expired > 0 || republished > 0 {
			_, _ = fmt.Fprintf(warn, "approvals: %d expired, %d re-published\n",
				expired, republished)
		}

		if raised, err := engine.SweepDrift(ctx); err != nil {
			if ctx.Err() == nil {
				_, _ = fmt.Fprintf(warn, "WARNING: collect drift: %v\n", err)
			}
		} else if raised > 0 {
			_, _ = fmt.Fprintf(warn, "drift: %d workspace(s) planned changes\n", raised)
		}

		if raised, err := engine.SweepChecks(ctx); err != nil {
			if ctx.Err() == nil {
				_, _ = fmt.Fprintf(warn, "WARNING: sweep checks: %v\n", err)
			}
		} else if raised > 0 {
			_, _ = fmt.Fprintf(warn, "checks: %d transition(s)\n", raised)
		}

		if paged, err := engine.SweepJobOutcomes(ctx); err != nil {
			if ctx.Err() == nil {
				_, _ = fmt.Fprintf(warn, "WARNING: page job failures: %v\n", err)
			}
		} else if paged > 0 {
			_, _ = fmt.Fprintf(warn, "job failures: %d paged\n", paged)
		}

		// After the failure sweep, so a requested job that also failed is already
		// checkpointed and this one stays silent rather than paging it twice.
		if paged, err := engine.SweepJobNotifyRequests(ctx); err != nil {
			if ctx.Err() == nil {
				_, _ = fmt.Fprintf(warn, "WARNING: page notify requests: %v\n", err)
			}
		} else if paged > 0 {
			_, _ = fmt.Fprintf(warn, "notify requests: %d paged\n", paged)
		}

		if paged, err := engine.SweepSecretExpiry(ctx); err != nil {
			if ctx.Err() == nil {
				_, _ = fmt.Fprintf(warn, "WARNING: page secret expiry: %v\n", err)
			}
		} else if paged > 0 {
			_, _ = fmt.Fprintf(warn, "secret expiry: %d paged\n", paged)
		}
	}
}

// flagStaleLocks names every workspace whose state lock outlived the job that
// took it. Breaking one is the operator's call, not the daemon's — an apply
// that is still running somewhere would lose its state to an automatic
// release — so this reports and points at the command (Decision 0004).
//
// It runs after the boot sweep, so a lock whose job that sweep just retired is
// already visible as stale.
func flagStaleLocks(ctx context.Context, st *store.Store, warn io.Writer) error {
	stale, err := st.StaleTFLocks(ctx)
	if err != nil {
		return err
	}
	for _, lock := range stale {
		ws, err := st.GetWorkspace(ctx, lock.WorkspaceID)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(warn, "WARNING: workspace %s holds a stale state lock (%s by %s since %s); "+
			"run `exocomp workspace unlock %s`\n",
			ws.Name, lock.Operation, lock.Who,
			lock.CreatedAt.Format("2006-01-02 15:04:05Z"), ws.Name)
	}
	return nil
}

// serveUntilSignal blocks until SIGINT or SIGTERM, rebuilding the daemon's
// reloadable sets on SIGHUP and on the reload interval meanwhile. A schedule —
// or a sealed push transport — added, enabled, or changed through the CLI
// reaches a running daemon exactly here. webErr is
// the web surface's serve-error channel (nil when no web surface was
// started): a post-bind failure there — a bad TLS config, a lost listener —
// is fatal, not something to keep running past, so it ends the wait and
// returns the error instead of the normal nil.
//
// reload is the one reload seam, shared with the operator API's `reload`
// (cordonController.reloadAll): it rebuilds the scheduler registrations and the
// notify transports together, so a signal and a `reload` verb never drift.
//
// restart is the drain controller's restart-ready channel (nil when no restart
// was requested): when it fires the daemon has drained and the caller must shut
// down cleanly and then re-exec. restarting reports that outcome so runServe can
// tell an ordinary stop from a restart.
func serveUntilSignal(sigCtx, jobCtx context.Context, out, warn io.Writer,
	hup <-chan os.Signal, reload func(context.Context) error, interval time.Duration, webErr <-chan error,
	restart <-chan struct{}) (restarting bool, err error) {
	var tick <-chan time.Time
	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		tick = ticker.C
	}

	for {
		select {
		case <-sigCtx.Done():
			return false, nil
		case <-restart:
			return true, nil
		case err := <-webErr:
			if err == nil || errors.Is(err, http.ErrServerClosed) {
				continue
			}
			return false, err
		case <-hup:
			if err := reload(jobCtx); err != nil {
				_, _ = fmt.Fprintf(warn, "WARNING: reload: %v\n", err)
				continue
			}
			_, _ = fmt.Fprintln(out, "schedules reloaded")
		case <-tick:
			if err := reload(jobCtx); err != nil {
				_, _ = fmt.Fprintf(warn, "WARNING: reload: %v\n", err)
			}
		}
	}
}

// cordonController is the daemon's drain seam (web.Maintenance). Cordon pauses
// the scheduler and closes the queue's claim gate, so a planned reboot stops
// taking new work while jobs already running finish; State answers the
// readiness the `drain` CLI polls. It is deliberately NOT a queue job: a cordon
// is control-plane state, not a run, so it has no Spec and no ledger entry.
//
// It also owns restart-after-drain: Restart cordons and returns at once, and a
// goroutine waits for the in-flight count to reach zero (the requesting job
// included, which is what keeps a job that asks for a restart from deadlocking
// against the daemon), then closes restartReady. runServe selects on that
// channel, shuts down, and re-execs.
type cordonController struct {
	store *store.Store

	mu    sync.Mutex
	on    bool
	sched *sched.Scheduler
	// reloadExtra refreshes a read-once set a reload owns beyond the scheduler
	// registrations — the notify transports. It is set once, after the daemon
	// builds that set, and read under mu.
	reloadExtra func(context.Context) error

	// Restart bookkeeping, guarded by mu.
	restartPending  bool
	restartFired    bool
	restartDeadline time.Time
	restartErr      error
	// restartReady is closed once the drain is complete and the restart should
	// fire. It is created once per process (a daemon restarts at most once), so
	// serveUntilSignal can select on it from the start.
	restartReady chan struct{}

	// Restart seams. newCordonController sets the production values; tests
	// override them to drive the drain without a real binary or scheduler.
	restartTimeout time.Duration
	restartPoll    time.Duration
	restartGate    func() error
	restartCancel  context.CancelFunc
}

// newCordonController builds the daemon's drain seam. cancel is the job
// context's cancel, the --force lever that cancels the jobs a drain is still
// waiting on.
func newCordonController(st *store.Store, cancel context.CancelFunc) *cordonController {
	return &cordonController{
		store:          st,
		restartReady:   make(chan struct{}),
		restartTimeout: restartDrainTimeout,
		restartPoll:    restartPollInterval,
		restartGate:    defaultRestartGate,
		restartCancel:  cancel,
	}
}

// claimAllowed is the queue's claim gate: closed (returns false) while cordoned.
func (c *cordonController) claimAllowed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.on
}

// setScheduler wires the scheduler handle once it exists, under mu: the web
// surface is already serving when runServe reaches this point, so an unlocked
// write would race Cordon/Uncordon/Reload reading c.sched and let a request in
// that window read nil and skip the scheduler pause while reporting cordoned.
func (c *cordonController) setScheduler(s *sched.Scheduler) {
	c.mu.Lock()
	c.sched = s
	c.mu.Unlock()
}

// setReloadExtra registers the refresh of a read-once set a reload owns beyond
// the scheduler registrations. It is called once the daemon has built that set
// (the notify transports), under mu, so a reload racing startup sees either no
// extra refresh or a complete one.
func (c *cordonController) setReloadExtra(fn func(context.Context) error) {
	c.mu.Lock()
	c.reloadExtra = fn
	c.mu.Unlock()
}

// Cordon stops new schedule fires and new job claims. It is idempotent.
func (c *cordonController) Cordon(ctx context.Context) (web.MaintenanceState, error) {
	c.mu.Lock()
	c.on = true
	s := c.sched
	c.mu.Unlock()
	if s != nil {
		s.Pause()
	}
	return c.state(ctx)
}

// Uncordon re-enables firing (with catch-up) and claiming. It is idempotent.
func (c *cordonController) Uncordon(ctx context.Context) (web.MaintenanceState, error) {
	c.mu.Lock()
	c.on = false
	s := c.sched
	c.mu.Unlock()
	if s != nil {
		s.Resume()
	}
	return c.state(ctx)
}

// State reports the current readiness.
func (c *cordonController) State(ctx context.Context) (web.MaintenanceState, error) {
	return c.state(ctx)
}

// Reload rebuilds the daemon's reloadable read-once sets from the database
// through reloadAll — the same path SIGHUP and the reload interval drive — and
// reports how many enabled schedules the store holds. That count is an upper
// bound on the live set: a row the cron parser rejects is logged and left out
// of the registrations, so the store can hold more enabled rows than the daemon
// actually registered. A reload that cannot read the database leaves the
// running sets alone (the scheduler builds the new set whole before swapping,
// and the extra refresh swaps only on success). A nil scheduler (the web
// surface starts before runServe wires the handle) is refused, which the
// handler renders as a 503.
func (c *cordonController) Reload(ctx context.Context) (web.ReloadState, error) {
	if err := c.reloadAll(ctx); err != nil {
		return web.ReloadState{}, err
	}
	rows, err := c.store.ListSchedules(ctx, true)
	if err != nil {
		return web.ReloadState{}, err
	}
	return web.ReloadState{Schedules: len(rows)}, nil
}

// reloadAll refreshes every reloadable read-once set through the one seam both
// SIGHUP/--reload-interval and the operator `reload` verb use: the scheduler
// registrations and the registered extra refresh (the notify transports). A
// failure leaves the running sets alone — the scheduler builds the new set whole
// before swapping, and the extra refresh swaps only after its rebuild succeeds
// — so a reload that cannot read the database never tears down what is running.
func (c *cordonController) reloadAll(ctx context.Context) error {
	c.mu.Lock()
	s := c.sched
	extra := c.reloadExtra
	c.mu.Unlock()
	if s == nil {
		return web.ErrNoScheduler
	}
	if err := s.Reload(ctx); err != nil {
		return err
	}
	if extra != nil {
		if err := extra(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Restart cordons the daemon and returns at once — the request is a one-way
// signal, never a wait. A goroutine then waits for the in-flight count to reach
// zero, bounded by restartTimeout; when it does, restartReady closes and the
// serve loop shuts down and re-execs. The requesting job counts as in flight
// until it is terminal, so a job that asks for its own restart drains before the
// daemon goes down instead of deadlocking against it.
//
// A restart is refused as ErrNothingToRestart when the on-disk binary is the
// running image, and a precheck that the on-disk binary runs is refused before
// anything is cordoned-for-keeps. On either refusal, or on a timeout, the cordon
// is lifted so the daemon keeps taking work. A second request while one is
// pending is a no-op.
func (c *cordonController) Restart(ctx context.Context, force bool) (web.RestartState, error) {
	c.mu.Lock()
	if c.restartPending {
		c.mu.Unlock()
		return c.RestartStatus(ctx)
	}
	// Reserve the restart while still holding mu, so a concurrent second request
	// sees it pending and returns the current status instead of running the
	// gate a second time. The gate runs outside the lock (it execs the on-disk
	// binary), but the pending flag is already set when we drop it.
	if c.restartTimeout <= 0 {
		c.restartTimeout = restartDrainTimeout
	}
	if c.restartPoll <= 0 {
		c.restartPoll = restartPollInterval
	}
	c.restartPending = true
	c.restartDeadline = time.Now().Add(c.restartTimeout)
	c.mu.Unlock()

	// Cordon first, reusing the one drain mechanism, so a gate failure below can
	// lift exactly the cordon this call set.
	if _, err := c.Cordon(ctx); err != nil {
		c.clearRestart()
		return web.RestartState{}, err
	}

	if gate := c.restartGate; gate != nil {
		if err := gate(); err != nil {
			_, _ = c.Uncordon(ctx)
			c.clearRestart()
			return web.RestartState{}, err
		}
	}

	go c.awaitRestart(force)
	return c.RestartStatus(ctx)
}

// clearRestart releases the pending reservation a refused restart took, so a
// later request starts clean rather than being told a restart is already
// pending.
func (c *cordonController) clearRestart() {
	c.mu.Lock()
	c.restartPending = false
	c.restartDeadline = time.Time{}
	c.mu.Unlock()
}

// RestartStatus reports a pending restart's drain progress.
func (c *cordonController) RestartStatus(ctx context.Context) (web.RestartState, error) {
	n, err := runningJobsOwnedByThisProcess(ctx, c.store)
	if err != nil {
		return web.RestartState{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := web.RestartState{Pending: c.restartPending, InFlight: n}
	if c.restartPending {
		st.Deadline = c.restartDeadline
	}
	return st, nil
}

// awaitRestart is the background half of a restart. It waits for the in-flight
// count to reach zero and fires, or aborts on the deadline. With force it
// cancels the remaining jobs and fires at once, so an operator who does not want
// to wait still gets a restart rather than a silent abort. A count error aborts
// rather than firing on an unknown state.
func (c *cordonController) awaitRestart(force bool) {
	ctx := context.Background()
	deadline := c.deadline()

	if force {
		c.cancelInFlight()
		c.fireRestart()
		return
	}

	for {
		n, err := runningJobsOwnedByThisProcess(ctx, c.store)
		if err != nil {
			c.abortRestart(fmt.Errorf("restart: count in-flight jobs: %w", err))
			return
		}
		if n == 0 {
			c.fireRestart()
			return
		}
		if !time.Now().Before(deadline) {
			c.abortRestart(errRestartTimedOut)
			return
		}
		time.Sleep(c.restartPoll)
	}
}

// deadline returns the drain deadline under the lock, defaulting to the
// configured timeout from now when an unset timeout slipped through.
func (c *cordonController) deadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.restartDeadline.IsZero() {
		return c.restartDeadline
	}
	return time.Now().Add(c.restartTimeout)
}

// cancelInFlight calls the daemon's job cancel (the --force lever). It is
// idempotent and safe with a nil seam (a controller with no queue).
func (c *cordonController) cancelInFlight() {
	c.mu.Lock()
	cancel := c.restartCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// fireRestart closes restartReady exactly once, waking the serve loop.
func (c *cordonController) fireRestart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.restartPending || c.restartFired {
		return
	}
	c.restartFired = true
	close(c.restartReady)
}

// abortRestart clears the pending flag, records the reason, and lifts the
// cordon — the fail-safe ending for a drain that could not complete.
func (c *cordonController) abortRestart(err error) {
	c.mu.Lock()
	if !c.restartPending || c.restartFired {
		c.mu.Unlock()
		return
	}
	c.restartPending = false
	c.restartErr = err
	c.mu.Unlock()
	_, _ = c.Uncordon(context.Background())
}

// errRestartTimedOut is the abort reason a `restart --wait` reads as a still-
// running daemon: the drain hit its deadline with jobs left, so the cordon was
// lifted rather than cancelling them.
var errRestartTimedOut = errors.New("restart: timed out waiting for in-flight jobs; cordon lifted")

// restartGate refuses a restart that would do nothing and prechecks the binary
// that would replace this process. The no-op test compares the running image
// (the /proc/self/exe inode, which survives an in-place upgrade) to the file at
// the on-disk path; the precheck runs the on-disk binary's `version` so a
// broken replacement is refused while the running process stays up.
//
// The comparison is best-effort: when either image cannot be read (no
// /proc/self/exe, an unreadable path) it proceeds rather than refusing, because
// a real restart must not be blocked by a hashing failure. The precheck is the
// hard gate, so a path that cannot even be resolved is a refusal — there is
// nothing to precheck and a restart must not proceed on an unverified binary.
func defaultRestartGate() error {
	disk, err := onDiskBinaryPathFn()
	if err != nil {
		return fmt.Errorf("restart: resolve on-disk binary: %w", err)
	}
	running, rerr := runningImageHash()
	ondisk, derr := fileHash(disk)
	if rerr == nil && derr == nil && running == ondisk {
		return web.ErrNothingToRestart
	}
	return precheckBinary(disk)
}

// onDiskBinaryPathFn is onDiskBinaryPath behind a variable so a test can force
// the resolve to fail and prove the gate refuses rather than skipping the
// precheck.
var onDiskBinaryPathFn = onDiskBinaryPath

// onDiskBinaryPath is the file a restart would exec: the running binary's own
// path, symlinks resolved, which is what an in-place upgrade replaces.
func onDiskBinaryPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, nil
}

// runningImageHash hashes the image this process is executing. /proc/self/exe
// is preferred because it keeps pointing at the running inode even after the
// on-disk path was replaced; os.Executable is the fallback on a host without
// procfs.
func runningImageHash() (string, error) {
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		self, eerr := os.Executable()
		if eerr != nil {
			return "", err
		}
		f, err = os.Open(self)
		if err != nil {
			return "", err
		}
	}
	defer func() { _ = f.Close() }()
	return hashReader(f)
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return hashReader(f)
}

func hashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// precheckBinary runs the on-disk binary's `version` before trusting it to
// replace this process. A non-zero exit or empty output is a refusal.
func precheckBinary(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("on-disk binary failed its precheck (`version`): %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) == "" {
		return errors.New("on-disk binary failed its precheck: `version` printed nothing")
	}
	return nil
}

func (c *cordonController) state(ctx context.Context) (web.MaintenanceState, error) {
	c.mu.Lock()
	on := c.on
	c.mu.Unlock()
	n, err := runningJobsOwnedByThisProcess(ctx, c.store)
	if err != nil {
		return web.MaintenanceState{}, err
	}
	return web.MaintenanceState{Cordoned: on, InFlight: n}, nil
}

// runningJobsOwnedByThisProcess counts the jobs this daemon has in the running
// state. In-flight is the reboot-blocking set: a queued row is not claimed while
// cordoned (and is swept to interrupted on the next boot), but a running row is
// live work whose terminal write a reboot would cut off.
func runningJobsOwnedByThisProcess(ctx context.Context, st *store.Store) (int, error) {
	rows, err := st.ListJobs(ctx, store.JobFilter{States: []store.JobState{store.StateRunning}})
	if err != nil {
		return 0, err
	}
	pid := os.Getpid()
	n := 0
	for _, job := range rows {
		if job.OwnerPID == pid {
			n++
		}
	}
	return n, nil
}

// shutdown is what stopping the daemon needs a handle on.
type shutdown struct {
	store      *store.Store
	sched      *sched.Scheduler
	server     *api.Server
	web        *webSurface
	cancelJobs context.CancelFunc
	queueDone  <-chan error
	grace      time.Duration
}

// shutdownServe stops the daemon in the one order that leaves the database
// consistent: the scheduler first, so nothing new fires; then the jobs already
// running, which get the grace window to finish their own terminal writes
// before anything cancels them; then whatever is still queued, retired to
// interrupted so no row waits on a process that is about to be gone; and the
// state backend last, because a tofu leg is still pushing state to it.
func shutdownServe(ctx context.Context, out, warn io.Writer, sd shutdown) error {
	sd.sched.Stop()

	left, err := waitForOwnJobs(ctx, sd.store, sd.grace)
	if err != nil {
		_, _ = fmt.Fprintf(warn, "WARNING: wait for in-flight jobs: %v\n", err)
	}
	if len(left) > 0 {
		_, _ = fmt.Fprintf(warn, "WARNING: %d job(s) unfinished after %s; cancelling\n",
			len(left), sd.grace)
	}
	sd.cancelJobs()

	var qErr error
	select {
	case qErr = <-sd.queueDone:
	case <-time.After(shutdownGrace):
		qErr = fmt.Errorf("serve: queue did not stop within %s", shutdownGrace)
	}

	retired, retireErr := retireOwnJobs(ctx, sd.store)
	if retired > 0 {
		_, _ = fmt.Fprintf(out, "retired %d job(s) this process owned to interrupted\n", retired)
	}

	// A CLI killed while its row waited on a key this daemon held leaves that
	// row queued and owned by nobody. The daemon is the last process here that
	// can see it; the next boot's sweep is too late to be the only answer.
	rec, sweepErr := sd.store.ReconcileNonTerminal(ctx)
	if gone := rec.Interrupted + rec.Orphaned; gone > 0 {
		_, _ = fmt.Fprintf(out, "retired %d job(s) left by a process that is gone to interrupted\n", gone)
	}
	if settled := rec.Released + rec.Cancelled; settled > 0 {
		_, _ = fmt.Fprintf(out, "reconciled %d job(s) whose approval decision was interrupted\n", settled)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, shutdownGrace)
	defer cancel()
	joined := errors.Join(qErr, retireErr, sweepErr, sd.web.Shutdown(shutdownCtx), sd.server.Shutdown(shutdownCtx))
	// The queue and its in-flight store reads stop because we cancelled them:
	// a context.Canceled from a deliberate shutdown is the expected outcome,
	// not a failure to report. A real timeout surfaces as its own message above.
	if joined != nil && errors.Is(joined, context.Canceled) && !hasNonCancelError(joined) {
		return nil
	}
	return joined
}

// hasNonCancelError reports whether a joined error carries anything other than
// context.Canceled, so shutdown reports genuine failures but not its own
// cancellation.
func hasNonCancelError(err error) bool {
	type unwrapper interface{ Unwrap() []error }
	if j, ok := err.(unwrapper); ok {
		for _, e := range j.Unwrap() {
			if e != nil && !errors.Is(e, context.Canceled) {
				return true
			}
		}
		return false
	}
	return !errors.Is(err, context.Canceled)
}

// waitForOwnJobs blocks until this process owns no queued or running job, or
// until grace runs out, and returns whatever is left. Ownership is the row's
// own owner_pid — the fact the boot sweep decides on — so a job another
// process is draining never holds this one's shutdown open.
func waitForOwnJobs(ctx context.Context, st *store.Store, grace time.Duration) ([]store.Job, error) {
	deadline := time.Now().Add(grace)
	for {
		mine, err := ownJobs(ctx, st)
		if err != nil || len(mine) == 0 {
			return mine, err
		}
		if !time.Now().Before(deadline) {
			return mine, nil
		}
		time.Sleep(drainPoll)
	}
}

// retireOwnJobs moves the rows this process still owns to interrupted, the
// terminal state a job that nothing will finish belongs in. Without it the
// next boot's sweep would do the same work, with the queued jobs of a stopped
// daemon looking runnable in between.
func retireOwnJobs(ctx context.Context, st *store.Store) (int, error) {
	mine, err := ownJobs(ctx, st)
	if err != nil {
		return 0, err
	}

	retired := 0
	var errs []error
	for _, job := range mine {
		err := st.TransitionJob(ctx, job.ID, job.State, store.StateInterrupted)
		switch {
		case err == nil:
			retired++
		case errors.Is(err, store.ErrStateConflict):
			// A worker finished it between the read and the write.
		default:
			errs = append(errs, err)
		}
	}
	return retired, errors.Join(errs...)
}

// ownJobs returns the queued and running jobs whose owner_pid is this process.
func ownJobs(ctx context.Context, st *store.Store) ([]store.Job, error) {
	rows, err := st.ListJobs(ctx, store.JobFilter{
		States: []store.JobState{store.StateQueued, store.StateRunning},
	})
	if err != nil {
		return nil, err
	}

	pid := os.Getpid()
	var mine []store.Job
	for _, job := range rows {
		if job.OwnerPID == pid {
			mine = append(mine, job)
		}
	}
	return mine, nil
}

// serveDriver builds the driver the daemon's queue runs jobs through. The
// daemon claims whatever is queued, not only what its own scheduler fires, so
// it has to serve every kind that can reach the queue while it is up.
func serveDriver(dir string, st *store.Store, env *secrets.Envelope, srv *api.Server) (executor.Driver, error) {
	dirs, err := tofuDirs(dir)
	if err != nil {
		return nil, err
	}
	quiesce, err := backup.LoadQuiesce(filepath.Join(dir, ".exocomp", "backup-quiesce.yml"))
	if err != nil {
		return nil, err
	}
	return kindRouter{
		tofu:    workspaceDriver{store: st, env: env, server: srv, dirs: dirs},
		ansible: ansibleJobDriver{store: st, env: env, dataDir: dir},
		cli:     cliJobDriver{store: st, env: env, dataDir: dir},
		hook:    hookJobDriver{store: st, env: env, dataDir: dir},
		verify:  verifyJobDriver{},
		// The doctor self-check runs the probe in-process over the daemon's
		// already-open store — the same store every other probe reads, with a
		// no-op close for the probe seam.
		doctor: health.NewJobDriver(dir, func() (health.Store, func() error, error) {
			return st, func() error { return nil }, nil
		}),
		rest: sched.NewDriver(executor.NewScopedShell(), backup.NewRunner(st, env, backup.Options{Quiesce: quiesce}, targetEnv)),
	}, nil
}

// kindRouter sends tofu, ansible, and cli jobs to the drivers that rebuild
// their per-job secret material, hook and gate steps to the run-lifecycle
// drivers, and everything else to the scheduler's shell/backup routing.
type kindRouter struct {
	tofu    executor.Driver
	ansible executor.Driver
	cli     executor.Driver
	hook    executor.Driver
	verify  executor.Driver
	doctor  executor.Driver
	rest    executor.Driver
}

// Run implements executor.Driver.
func (r kindRouter) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	switch spec.Kind {
	case executor.KindTofu:
		return r.tofu.Run(ctx, spec, sink)
	case executor.KindAnsible:
		return r.ansible.Run(ctx, spec, sink)
	case executor.KindCLI:
		return r.cli.Run(ctx, spec, sink)
	case hooks.JobKind:
		return r.hook.Run(ctx, spec, sink)
	case verify.JobKind:
		return r.verify.Run(ctx, spec, sink)
	case health.JobKind:
		return r.doctor.Run(ctx, spec, sink)
	default:
		return r.rest.Run(ctx, spec, sink)
	}
}

// cliJobDriver runs a queued governed-cli job the daemon claimed. The
// allow-list entry is deliberately absent from the job row beyond its
// name — spec.Argv[0] is the registered tool, the CLIArgv convention — so the
// driver is rebuilt per job from the entry the store holds right now, the
// same shape workspaceDriver and ansibleJobDriver rebuild their own options
// in. An entry deregistered between enqueue and claim is refused here rather
// than run: this never falls back to a raw binary.
type cliJobDriver struct {
	store   *store.Store
	env     *secrets.Envelope
	dataDir string
}

// Run implements executor.Driver.
func (d cliJobDriver) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	job, err := d.store.GetJob(ctx, spec.JobID)
	if err != nil {
		return executor.Result{}, err
	}
	if len(spec.Argv) == 0 {
		return executor.Result{}, fmt.Errorf("serve: job %d: cli job carries no argv", job.ID)
	}

	tool, err := d.store.CLIToolByName(ctx, spec.Argv[0])
	if errors.Is(err, store.ErrNotFound) {
		return executor.Result{}, fmt.Errorf(
			"serve: job %d: cli tool %q is no longer on the allow-list; refusing to run",
			job.ID, spec.Argv[0])
	}
	if err != nil {
		return executor.Result{}, err
	}

	runSecrets, secretPlain, err := resolveRunSecrets(ctx, d.store, d.env, job.Args.RunSecretBinding, "")
	if err != nil {
		return executor.Result{}, err
	}

	caps, err := resolveRunnerCaps(ctx, d.store, job.Args.RunnerName)
	if err != nil {
		return executor.Result{}, err
	}
	drv, err := cliTierDriver(d.dataDir, executor.CLIOptions{
		Tool:       tool.Name,
		Binary:     tool.Binary,
		RunSecrets: runSecrets,
		AllowHTTPS: tool.Egress == store.CLIEgressHTTPS,
	}, caps)
	if err != nil {
		return executor.Result{}, err
	}
	if len(secretPlain) == 0 {
		return drv.Run(ctx, spec, sink)
	}
	return drv.Run(ctx, spec, runSink{redactor: secrets.NewRedactor(secretPlain), next: sink})
}

// workspaceDriver runs a queued tofu job. What such a job needs beyond its
// argv — the backend bearer and the state passphrase — is derived from the
// envelope at execution time and deliberately absent from the job row, so the
// driver is rebuilt per job from the workspace the runbook names.
type workspaceDriver struct {
	store  *store.Store
	env    *secrets.Envelope
	server *api.Server
	dirs   tofuPaths
}

// Run implements executor.Driver.
func (d workspaceDriver) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	job, err := d.store.GetJob(ctx, spec.JobID)
	if err != nil {
		return executor.Result{}, err
	}
	name, ok := strings.CutPrefix(job.Runbook, workspaceRunbookPrefix)
	if !ok || name == "" {
		return executor.Result{}, fmt.Errorf("serve: job %d names no workspace (runbook %q)",
			job.ID, job.Runbook)
	}
	ws, err := d.store.WorkspaceByName(ctx, name)
	if err != nil {
		return executor.Result{}, fmt.Errorf("serve: workspace %q: %w", name, err)
	}

	token := api.WorkspaceToken(d.env, ws.Name)
	stateKey := d.env.Derive(executor.TofuStatePurpose, ws.Name)
	passphrase := base64.StdEncoding.EncodeToString(stateKey)

	runSecrets, secretPlain, err := resolveRunSecrets(ctx, d.store, d.env, job.Args.RunSecretBinding, "")
	if err != nil {
		return executor.Result{}, err
	}

	d.server.BindJob(ws.Name, job.ID)
	defer d.server.ReleaseJob(ws.Name)

	op := tofuOp(spec.Argv)

	// An apply of a sealed saved plan needs the plaintext for the one run that
	// consumes it. A pure-enqueue face (the API) enqueues the sealed path because
	// it cannot hold a decrypted copy until a later run reads it; the daemon opens
	// it here, into the job's private dir, removed when the run ends.
	if op == "apply" {
		if plan, ok := applyPlanArg(spec.Argv); ok {
			readable, release, oerr := service.OpenPlanForRun(d.env, d.dirs.private, plan)
			if oerr != nil {
				return executor.Result{}, oerr
			}
			defer release()
			if readable != plan {
				spec.Argv = setApplyPlanArg(spec.Argv, readable)
			}
		}
	}

	caps, err := resolveRunnerCaps(ctx, d.store, job.Args.RunnerName)
	if err != nil {
		return executor.Result{}, err
	}
	drv, err := tofuTierDriver(d.dirs.root, executor.TofuOptions{
		Workspace:       ws.Name,
		BackendURL:      api.StateURL(d.server.Addr(), ws.Name),
		BackendToken:    token,
		StatePassphrase: passphrase,
		PrivateDirRoot:  d.dirs.private,
		PluginCacheDir:  d.dirs.pluginCache,
		RunSecrets:      runSecrets,
		// A bootstrap chain's --capture-file names, via the step's Captures field,
		// the run-secrets the apply must receive as files staged through the
		// resolved secret-staging backend (executor.ConfigureRunSecretStaging,
		// above) rather than as env values. Empty for every other apply.
		RunSecretFiles: bootstrapStagedFileEnvs(job.Args.Captures),
		// The workspace's opt-in egress widening (docs/operations/serve.md);
		// empty by default, so the daemon path keeps the 443-and-backend grant.
		ExtraPorts: ws.ExtraEgressPorts,
		// Every successful apply materializes the workspace's declared,
		// non-sensitive outputs as an inventory artifact. The path is
		// deterministic so the dependent play resolves it, and each apply
		// refreshes the same file.
		InventoryOut: handoffInventoryPath(d.dirs.root, ws.Name),
		// Sensitive outputs seal and bind the same way as the daemonless apply
		// path (runWorkspaceInline): without this, a daemon-driven apply leaves
		// the tf:<workspace> binding empty and the dependent play finds nothing.
		OutputSealer: executor.EnvelopeOutputSealer{Env: d.env, Store: d.store, Workspace: ws.Name},
		// The workspace's offline-provider posture (workspace offline on):
		// resolve providers from the local mirror alone, no registry access.
		OfflineProviders: ws.OfflineProviders,
	}, caps)
	if err != nil {
		return executor.Result{}, err
	}
	material := append([][]byte{[]byte(token), stateKey, []byte(passphrase)}, secretPlain...)
	redactor := secrets.NewRedactor(material)
	res, err := drv.Run(ctx, spec, runSink{redactor: redactor, next: sink})
	if err == nil && res.ExitCode == 0 && op == "apply" {
		if recErr := recordInventoryArtifact(ctx, d.store, spec.JobID, ws.Name, d.dirs.root); recErr != nil {
			sink.Emit(executor.Event{
				Ts:    time.Now().UTC(),
				Level: executor.LevelError,
				Msg:   fmt.Sprintf("record inventory artifact: %v", recErr),
			})
		}
	}
	// A plan the API or the TUI asked for cannot be sealed by its caller, which
	// only enqueued the row; the daemon seals its -out into a consumable artifact
	// and records its policy verdict, so a later apply-by-id can consult it. A
	// CLI plan carries the manual trigger and seals its own, so this runs for the
	// "api" and "tui" triggers alone.
	if err == nil && op == "plan" &&
		(job.Trigger == service.TriggerAPI || job.Trigger == service.TriggerTUI) {
		d.sealAPIPlan(ctx, job, ws, spec.Argv, sink)
	}
	return res, err
}

// sealAPIPlan seals an API-initiated plan's -out file into a consumable artifact
// and records its policy verdict, through the same service verbs the CLI's
// post-run half calls. A failure is loud but never fails the run: the plan
// itself already succeeded (exit-code doctrine), so a seal fault must not turn a
// good plan into a failed job — it only leaves the plan unconsumable by id.
func (d workspaceDriver) sealAPIPlan(ctx context.Context, job store.Job, ws store.Workspace,
	argv []string, sink executor.EventSink) {
	planPath := planOutArg(argv)
	if planPath == "" {
		return
	}
	emit := func(format string, a ...any) {
		sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: executor.LevelError, Msg: fmt.Sprintf(format, a...)})
	}
	svc := service.New(d.store, d.dirs.root, nil)
	// Adjudicate BEFORE the artifact row exists: the id an apply can name becomes
	// addressable only once the verdict is ready to stamp on it, so the window in
	// which an apply-by-id sees a sealed-but-undecided plan is one event append
	// wide. The apply gate fails closed on what is left of it.
	res, err := svc.EvalPlanPolicy(ctx, ws, job.ID)
	if err != nil {
		emit("evaluate plan policy: %v", err)
		return
	}
	if res.FailNotice != "" {
		sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: executor.LevelSystem, Msg: res.FailNotice})
	}
	saved, err := svc.SealPlan(ctx, d.env, job.ID, planPath, ws.Serial)
	if err != nil {
		emit("seal plan artifact: %v", err)
		return
	}
	if saved == nil {
		return
	}
	switch {
	case res.Verdict.Matched():
		if _, err := svc.RecordPlanVerdict(ctx, saved.JobID, saved.ID, res.Verdict); err != nil {
			emit("record plan verdict: %v", err)
		}
	case res.Evaluated:
		// Policies ran and none matched: record that, or the plan would read as
		// never-adjudicated and its apply would be refused.
		if err := svc.RecordPlanAdjudication(ctx, saved.JobID, saved.ID); err != nil {
			emit("record plan adjudication: %v", err)
		}
	}
}

// tofuOp is the operation a tofu argv names, or "" when it names none.
func tofuOp(argv []string) string {
	if len(argv) < 2 {
		return ""
	}
	return argv[1]
}

// applyPlanArg reports the saved-plan file an `apply <planfile>` argv names, or
// false for a bare `apply -auto-approve` or any other operation.
func applyPlanArg(argv []string) (string, bool) {
	if len(argv) >= 3 && argv[1] == "apply" && !strings.HasPrefix(argv[2], "-") {
		return argv[2], true
	}
	return "", false
}

// setApplyPlanArg rewrites the plan file an apply reads, returning a copy so the
// caller's slice is left alone.
func setApplyPlanArg(argv []string, path string) []string {
	out := append([]string(nil), argv...)
	out[2] = path
	return out
}

// planOutArg is the `-out=PATH` a plan argv writes to, or "" when it names none.
func planOutArg(argv []string) string {
	for _, a := range argv {
		if v, ok := strings.CutPrefix(a, "-out="); ok {
			return v
		}
	}
	return ""
}

// runSink scrubs the material one run derives. The queue's own redactor is
// built once, from the sealed secrets; a workspace's bearer and passphrase
// exist only for the length of a job, so they are scrubbed here instead.
type runSink struct {
	redactor *secrets.Redactor
	next     executor.EventSink
}

// Emit implements executor.EventSink.
func (s runSink) Emit(ev executor.Event) {
	ev.Msg = string(s.redactor.Redact([]byte(ev.Msg)))
	if ev.RawJSON != "" {
		ev.RawJSON = string(s.redactor.Redact([]byte(ev.RawJSON)))
	}
	s.next.Emit(ev)
}
