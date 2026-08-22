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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/metrics"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
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

// serveOptions is what the daemon is started with.
type serveOptions struct {
	port           int
	drainGrace     time.Duration
	reloadInterval time.Duration
	sweepInterval  time.Duration
	dedupWindow    time.Duration
	approvalTTL    time.Duration
	hookAllow      string
	publicURL      string
	sandbox        string
}

func defaultServeOptions() serveOptions {
	return serveOptions{
		drainGrace:     defaultDrainGrace,
		reloadInterval: defaultReloadInterval,
		sweepInterval:  defaultSweepInterval,
		dedupWindow:    reactive.DefaultDedupWindow,
		approvalTTL:    reactive.DefaultApprovalTTL,
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
	cmd.Flags().StringVar(&opts.sandbox, "sandbox", "",
		"job isolation tier: local, local-sandboxed, or auto (default: $"+sandboxTierEnv+", else auto)")
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

	st, err := openJobStore(ctx, warn)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if err := flagStaleLocks(ctx, st, warn); err != nil {
		return err
	}

	env, err := secrets.Open(dir)
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
	q := queue.New(st, drv, queue.Options{Workers: serveWorkers, PollInterval: servePoll},
		queue.WithRedactorProvider(func() *secrets.Redactor {
			return secrets.NewRedactor(knownSecrets(jobCtx, st, env, warn))
		}))

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
	// second bind and a second auth story to get wrong.
	if err := srv.Mount(map[string]http.Handler{
		"GET /metrics": metrics.New(st, dir).Handler(),
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

	qDone := make(chan error, 1)
	go func() { qDone <- q.Run(jobCtx) }()
	_, _ = fmt.Fprintf(out, "queue draining with %d workers\n", serveWorkers)

	s := sched.New(st, q, nil)
	if err := s.Start(jobCtx); err != nil {
		cancelJobs()
		<-qDone
		return fmt.Errorf("serve: %w", err)
	}
	_, _ = fmt.Fprintln(out, "scheduler started")

	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		sweepReactive(jobCtx, engine, warn, opts.sweepInterval)
	}()

	serveUntilSignal(sigCtx, jobCtx, out, warn, hup, s, opts.reloadInterval)
	_, _ = fmt.Fprintln(out, "shutting down")

	err = shutdownServe(finalCtx, out, warn, shutdown{
		store:      st,
		sched:      s,
		server:     srv,
		cancelJobs: cancelJobs,
		queueDone:  qDone,
		grace:      opts.drainGrace,
	})
	<-sweepDone
	return err
}

// reactiveEngine builds the rule engine, with push configured from sealed
// secrets. An operator who has not sealed an ntfy endpoint gets a working
// daemon and a silent phone, not a daemon that refuses to start.
func reactiveEngine(ctx context.Context, st *store.Store, q *queue.Queue,
	env *secrets.Envelope, warn io.Writer, opts serveOptions) (*reactive.Engine, error) {
	transport, err := notify.LoadNtfy(ctx, func(ctx context.Context, name string) ([]byte, error) {
		row, err := st.GetSecret(ctx, name)
		if err != nil {
			return nil, err
		}
		return env.OpenSecret(secrets.SealedSecret{
			Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
		})
	})
	if err != nil {
		// A refused push configuration is the operator's to fix, and running
		// blind past it would mean approvals nobody is told about.
		return nil, fmt.Errorf("serve: %w", err)
	}

	var notifier *notify.Notifier
	if transport != nil {
		notifier = notify.New(nil, transport)
	} else {
		notifier = notify.New(nil)
		_, _ = fmt.Fprintf(warn, "WARNING: no push transport sealed (%s, %s); "+
			"approvals will wait silently\n", notify.SecretNtfyURL, notify.SecretNtfyTopic)
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
// one more push, and a scheduled plan that found changes becomes a drift
// event. It runs until the job context is cancelled.
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

// shutdown is what stopping the daemon needs a handle on.
type shutdown struct {
	store      *store.Store
	sched      *sched.Scheduler
	server     *api.Server
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
	return errors.Join(qErr, retireErr, sweepErr, sd.server.Shutdown(shutdownCtx))
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
	return kindRouter{
		tofu:    workspaceDriver{store: st, env: env, server: srv, dirs: dirs},
		ansible: ansibleJobDriver{store: st, env: env, dataDir: dir},
		rest:    sched.NewDriver(executor.NewShell(), backup.NewRunner(st, env, backup.Options{}, targetEnv)),
	}, nil
}

// kindRouter sends tofu and ansible jobs to the drivers that rebuild their
// per-job secret material, and everything else to the scheduler's shell/backup
// routing.
type kindRouter struct {
	tofu    executor.Driver
	ansible executor.Driver
	rest    executor.Driver
}

// Run implements executor.Driver.
func (r kindRouter) Run(ctx context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	switch spec.Kind {
	case executor.KindTofu:
		return r.tofu.Run(ctx, spec, sink)
	case executor.KindAnsible:
		return r.ansible.Run(ctx, spec, sink)
	default:
		return r.rest.Run(ctx, spec, sink)
	}
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

	d.server.BindJob(ws.Name, job.ID)
	defer d.server.ReleaseJob(ws.Name)

	drv := tofuTierDriver(d.dirs.root, executor.TofuOptions{
		Workspace:       ws.Name,
		BackendURL:      api.StateURL(d.server.Addr(), ws.Name),
		BackendToken:    token,
		StatePassphrase: passphrase,
		PrivateDirRoot:  d.dirs.private,
		PluginCacheDir:  d.dirs.pluginCache,
	})
	redactor := secrets.NewRedactor([][]byte{[]byte(token), stateKey, []byte(passphrase)})
	return drv.Run(ctx, spec, runSink{redactor: redactor, next: sink})
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
