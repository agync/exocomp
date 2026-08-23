package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
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

// defaultSweepInterval is how often the daemon fails expired approvals
// closed, re-publishes the ones halfway through their window, and turns a
// plan that found changes into a drift event.
const defaultSweepInterval = 30 * time.Second

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
	sandbox        string
	webAddr        string
	webInsecure    bool
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
	cmd.Flags().StringVar(&opts.webAddr, "web-addr", "",
		"also serve the token-gated LAN web+API surface here over TLS (ADR 0009); empty starts no web listener")
	cmd.Flags().BoolVar(&opts.webInsecure, "web-insecure-http", false,
		"serve the web surface as cleartext HTTP (loud, never the documented path)")
	cmd.Flags().StringVar(&opts.sandbox, "sandbox", "",
		"job isolation tier: local, local-sandboxed, or auto (default: $"+sandboxTierEnv+", else auto)")
	cmd.Flags().DurationVar(&opts.leaseTTL, "lease-ttl", opts.leaseTTL,
		"how long the cross-host active lease stays live without a renew")
	cmd.Flags().DurationVar(&opts.leaseRenew, "lease-renew", opts.leaseRenew,
		"how often the active daemon renews the cross-host lease (keep well below --lease-ttl)")
	cmd.Flags().BoolVar(&opts.forceLease, "force-lease", false,
		"take over an active lease still held by another daemon; use only when that holder is truly gone")
	return cmd
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

	if err := requestSandboxTier(opts.sandbox, warn); err != nil {
		return fmt.Errorf("serve: %w", err)
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

	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

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
	// exists (below). Until then it is uncordoned, so the gate is open.
	cordon := &cordonController{store: st}
	q := queue.New(st, drv, queue.Options{Workers: serveWorkers, PollInterval: servePoll},
		queue.WithRedactorProvider(func() *secrets.Redactor {
			return secrets.NewRedactor(knownSecrets(jobCtx, st, env, warn))
		}),
		queue.WithClaimGate(cordon.claimAllowed))

	// The reactive surfaces are mounted on the one listener before it binds:
	// routes are fixed at start, and a second listener would be a second
	// authentication story.
	engine, err := reactiveEngine(ctx, st, q, env, warn, opts)
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
	// (finding D3) with a token derived from the envelope — configure
	// Prometheus with `exocomp doctor`'s metrics-scrape credential.
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
	base := opts.publicURL
	if base == "" {
		base = "http://" + srv.Addr()
	}
	engine.SetBaseURL(base)
	_, _ = fmt.Fprintf(out, "state backend listening on http://%s\n", srv.Addr())
	printHooks(out, env, base)

	// The LAN web/token surface is a second, separate listener: non-loopback
	// and TLS, token-gated per ADR 0009, sharing this process's engine so an
	// approval decided in a browser releases the job into this queue. The
	// loopback state backend above stays loopback-only and takes no LAN token.
	var surface *webSurface
	if opts.webAddr != "" {
		surface, err = startWebSurface(ctx, st, env, dir, engine, cordon, opts.webAddr, opts.webInsecure, warn)
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

	s := sched.New(st, q, nil)
	if err := s.Start(jobCtx); err != nil {
		cancelJobs()
		<-qDone
		return fmt.Errorf("serve: %w", err)
	}
	cordon.sched = s
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
		sweepReactive(jobCtx, engine, warn, opts.sweepInterval)
	}()

	// The step-ledger sequencer runs here, after the boot job-sweep (openJobStore
	// retired interrupted rows) and after the queue's workers are draining: its
	// first pass is ResumeAll, so a workflow a crash left mid-flight continues
	// from the ledger, and it then keeps live workflows — the story-5 apply->play
	// handoff — progressing. The queue stays the sole executor; this only
	// releases each next step and records its outcome (ADR 0008).
	seq := queue.NewSequencer(q)
	seqDone := make(chan struct{})
	go func() {
		defer close(seqDone)
		driveWorkflows(jobCtx, seq, out, warn)
	}()

	serveUntilSignal(sigCtx, jobCtx, out, warn, hup, s, opts.reloadInterval)
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
	return err
}

// bootSweepJobs retires the jobs a process that is gone left queued or running,
// the same repair openJobStore runs for the CLIs — but serve runs it only after
// the cross-host fence is held, so a fenced-out standby never sweeps a live
// primary's rows. Its pid-liveness test is host-local by design (invariant 1:
// one host owns the store at a time), which the lease now enforces across hosts.
func bootSweepJobs(ctx context.Context, st *store.Store, warn io.Writer) error {
	swept, err := st.SweepInterrupted(ctx)
	if err != nil {
		return err
	}
	if swept > 0 && warn != nil {
		_, _ = fmt.Fprintf(warn, "swept %d job(s) left by a process that is gone to interrupted\n", swept)
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

// reactiveEngine builds the rule engine, with push configured from sealed
// secrets. An operator who has not sealed an ntfy endpoint gets a working
// daemon and a silent phone, not a daemon that refuses to start.
func reactiveEngine(ctx context.Context, st *store.Store, q *queue.Queue,
	env *secrets.Envelope, warn io.Writer, opts serveOptions) (*reactive.Engine, error) {
	look := func(ctx context.Context, name string) ([]byte, error) {
		row, err := st.GetSecret(ctx, name)
		if err != nil {
			return nil, err
		}
		return env.OpenSecret(secrets.SealedSecret{
			Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
		})
	}

	// Both transports are loaded and every non-nil one is used. A refused
	// configuration is the operator's to fix and hard-fails here — running blind
	// past it would mean approvals and failures nobody is told about — but an
	// unconfigured transport is a nil, not an error: an operator who sealed only
	// one, or neither, still gets a working daemon.
	var transports []notify.Transport

	ntfyT, err := notify.LoadNtfy(ctx, look)
	if err != nil {
		return nil, fmt.Errorf("serve: %w", err)
	}
	if ntfyT != nil {
		transports = append(transports, ntfyT)
	}

	goalertT, err := notify.LoadGoAlert(ctx, look)
	if err != nil {
		return nil, fmt.Errorf("serve: %w", err)
	}
	if goalertT != nil {
		transports = append(transports, goalertT)
	}

	notifier := notify.New(nil, transports...)
	if len(transports) == 0 {
		_, _ = fmt.Fprintf(warn, "WARNING: no push transport sealed "+
			"(ntfy: %s, %s; goalert: %s, %s); approvals and job failures will alert silently\n",
			notify.SecretNtfyURL, notify.SecretNtfyTopic,
			notify.SecretGoAlertURL, notify.SecretGoAlertKey)
	}

	return reactive.New(reactive.Options{
		Store:       st,
		Queue:       q,
		Deriver:     env,
		Notifier:    notifier,
		DedupWindow: opts.dedupWindow,
		ApprovalTTL: opts.approvalTTL,
		Redactor: func() reactive.Redactor {
			return secrets.NewRedactor(knownSecrets(ctx, st, env, warn))
		},
	})
}

// printHooks tells the operator where each source posts. The tokens are
// derived, so they are the same after a restore and this is the only place
// they have to be looked up.
func printHooks(out io.Writer, env *secrets.Envelope, base string) {
	for _, source := range reactive.Sources {
		_, _ = fmt.Fprintf(out, "hook %-14s %s\n", source, reactive.HookURL(base, env, source))
	}
}

// sweepReactive runs the clock-driven halves of the reactive layer: approvals
// whose TTL ran out fail closed, approvals halfway through their window get
// one more push, a scheduled plan that found changes becomes a drift event,
// a job that ended in a terminal failure pages the operator once, and a secret
// past (or nearing) its expiry pages once. It runs until the job context is
// cancelled.
func sweepReactive(ctx context.Context, engine *reactive.Engine, warn io.Writer, interval time.Duration) {
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

		if paged, err := engine.SweepJobOutcomes(ctx); err != nil {
			if ctx.Err() == nil {
				_, _ = fmt.Fprintf(warn, "WARNING: page job failures: %v\n", err)
			}
		} else if paged > 0 {
			_, _ = fmt.Fprintf(warn, "job failures: %d paged\n", paged)
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
// release — so this reports and points at the command (ADR 0004).
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

// serveUntilSignal blocks until SIGINT or SIGTERM, rebuilding the schedules on
// SIGHUP and on the reload interval meanwhile. A schedule added, enabled, or
// disabled through the CLI reaches a running daemon exactly here.
func serveUntilSignal(sigCtx, jobCtx context.Context, out, warn io.Writer,
	hup <-chan os.Signal, s *sched.Scheduler, interval time.Duration) {
	var tick <-chan time.Time
	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		tick = ticker.C
	}

	for {
		select {
		case <-sigCtx.Done():
			return
		case <-hup:
			if err := s.Reload(jobCtx); err != nil {
				_, _ = fmt.Fprintf(warn, "WARNING: reload schedules: %v\n", err)
				continue
			}
			_, _ = fmt.Fprintln(out, "schedules reloaded")
		case <-tick:
			if err := s.Reload(jobCtx); err != nil {
				_, _ = fmt.Fprintf(warn, "WARNING: reload schedules: %v\n", err)
			}
		}
	}
}

// cordonController is the daemon's drain seam (web.Maintenance). Cordon pauses
// the scheduler and closes the queue's claim gate, so a planned reboot stops
// taking new work while jobs already running finish; State answers the
// readiness the `drain` CLI polls. It is deliberately NOT a queue job: a cordon
// is control-plane state, not a run, so it has no Spec and no ledger entry.
type cordonController struct {
	store *store.Store

	mu    sync.Mutex
	on    bool
	sched *sched.Scheduler
}

// claimAllowed is the queue's claim gate: closed (returns false) while cordoned.
func (c *cordonController) claimAllowed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.on
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
	swept, sweepErr := sd.store.SweepInterrupted(ctx)
	if swept > 0 {
		_, _ = fmt.Fprintf(out, "retired %d job(s) left by a process that is gone to interrupted\n", swept)
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
		rest:    sched.NewDriver(executor.NewShell(), backup.NewRunner(st, env, backup.Options{Quiesce: quiesce}, targetEnv)),
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

	drv := cliTierDriver(d.dataDir, executor.CLIOptions{
		Tool:       tool.Name,
		Binary:     tool.Binary,
		RunSecrets: runSecrets,
		AllowHTTPS: tool.Egress == store.CLIEgressHTTPS,
	})
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

	drv := tofuTierDriver(d.dirs.root, executor.TofuOptions{
		Workspace:       ws.Name,
		BackendURL:      api.StateURL(d.server.Addr(), ws.Name),
		BackendToken:    token,
		StatePassphrase: passphrase,
		PrivateDirRoot:  d.dirs.private,
		PluginCacheDir:  d.dirs.pluginCache,
		RunSecrets:      runSecrets,
		// A bootstrap chain's --capture-file names, via the step's Captures field,
		// the run-secrets the apply must receive as tmpfs-staged files rather than
		// env values. Empty for every other apply.
		RunSecretFiles: bootstrapStagedFileEnvs(job.Args.Captures),
		// The workspace's opt-in egress widening (docs/operations/serve.md);
		// empty by default, so the daemon path keeps the 443-and-backend grant.
		ExtraPorts: ws.ExtraEgressPorts,
		// Every successful apply materializes the workspace's declared,
		// non-sensitive outputs as an inventory artifact (story 5). The path is
		// deterministic so the dependent play resolves it, and each apply
		// refreshes the same file.
		InventoryOut: handoffInventoryPath(d.dirs.root, ws.Name),
		// Sensitive outputs seal and bind the same way as the daemonless apply
		// path (runWorkspaceInline): without this, a daemon-driven apply leaves
		// the tf:<workspace> binding empty and the dependent play finds nothing.
		OutputSealer: executor.EnvelopeOutputSealer{Env: d.env, Store: d.store, Workspace: ws.Name},
	})
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
	// A plan the API asked for cannot be sealed by its caller, which only
	// enqueued the row; the daemon seals its -out into a consumable artifact and
	// records its policy verdict, so a later apply-by-id can consult it. A CLI
	// plan carries the manual trigger and seals its own, so this runs for the
	// "api" trigger alone.
	if err == nil && op == "plan" && job.Trigger == service.TriggerAPI {
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
	// wide (finding AI-3). The apply gate fails closed on what is left of it.
	res, err := svc.EvalPlanPolicy(ctx, ws, job.ID)
	if err != nil {
		emit("evaluate plan policy: %v", err)
		return
	}
	if res.FailNotice != "" {
		sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: executor.LevelSystem, Msg: res.FailNotice})
	}
	saved, err := svc.SealPlan(ctx, d.env, job.ID, planPath)
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
