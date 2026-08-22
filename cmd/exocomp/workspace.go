package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newWorkspaceCmd)
}

// planArtifactKind labels the saved plan file on both the job that produced it
// and the job that consumed it.
const planArtifactKind = "tofu-plan"

// workspaceRunbookPrefix is what `workspace plan|apply` puts in a job's
// runbook. It is the only place the workspace a queued tofu job belongs to is
// recorded, so the daemon reads it back to rebuild that job's driver.
const workspaceRunbookPrefix = "workspace:"

// planExitChanges is what a plan reports to the shell when it found changes,
// mirroring `tofu plan -detailed-exitcode`. The job itself succeeded: drift is
// a fact about infrastructure, not a failed run.
const planExitChanges = 2

func newWorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Manage OpenTofu workspaces and run them through the queue",
	}
	cmd.AddCommand(
		newWorkspaceAddCmd(),
		newWorkspaceListCmd(),
		newWorkspacePlanCmd(),
		newWorkspaceApplyCmd(),
		newWorkspaceUnlockCmd(),
		newWorkspacePruneCmd(),
	)
	return cmd
}

func newWorkspacePruneCmd() *cobra.Command {
	var (
		keep int
		days int
	)
	prune := &cobra.Command{
		Use:   "prune [name]",
		Short: "Drop old state serials past the keep policy",
		Long: "Deletes state versions a workspace no longer needs: the newest --keep " +
			"serials are always retained, and so is every version from the last --days. " +
			"A version is removed only when it is both older than the window and beyond " +
			"the serial budget, so a workspace never loses its current state. With no " +
			"name every workspace is pruned.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()

			var targets []store.Workspace
			if len(args) == 1 {
				ws, err := st.WorkspaceByName(ctx, args[0])
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("workspace prune: no workspace %q", args[0])
				}
				if err != nil {
					return err
				}
				targets = []store.Workspace{ws}
			} else {
				targets, err = st.ListWorkspaces(ctx)
				if err != nil {
					return err
				}
			}

			since := time.Now().AddDate(0, 0, -days)
			total := 0
			for _, ws := range targets {
				n, err := st.PruneTFStates(ctx, ws.ID, keep, since)
				if err != nil {
					return err
				}
				total += n
				_, _ = fmt.Fprintf(out, "%-20s pruned %d\n", ws.Name, n)
			}
			_, _ = fmt.Fprintf(out, "pruned %d state versions across %d workspaces\n", total, len(targets))
			return nil
		},
	}
	prune.Flags().IntVar(&keep, "keep", 10, "newest serials always retained")
	prune.Flags().IntVar(&days, "days", 90, "retain every version from this many days back")
	return prune
}

func newWorkspaceUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock <name>",
		Short: "Break a state lock whose job is gone",
		Long: "A job killed mid-apply leaves the workspace's state lock held by nobody, " +
			"and no run can take it again. This breaks it and records that on the job the " +
			"lock named, or on the workspace's most recent job. `exocomp serve` flags such " +
			"locks at startup; breaking one whose job is still running is refused.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceUnlock(cmd, args[0])
		},
	}
}

// runWorkspaceUnlock breaks one workspace's lock. The audit row is the point:
// a lock that vanished without a trace is indistinguishable from one that was
// never taken (ADR 0004).
func runWorkspaceUnlock(cmd *cobra.Command, name string) (err error) {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	ws, err := st.WorkspaceByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("workspace unlock: no workspace %q", name)
	}
	if err != nil {
		return err
	}

	held, err := st.GetTFLock(ctx, ws.ID)
	if errors.Is(err, store.ErrNotFound) {
		_, err = fmt.Fprintf(out, "workspace %s is not locked\n", ws.Name)
		return err
	}
	if err != nil {
		return err
	}

	// A live job's lock is the system working, not a leftover.
	if held.JobID != nil {
		owner, err := st.GetJob(ctx, *held.JobID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil && !owner.State.Terminal() {
			return fmt.Errorf("workspace unlock: job %d is still %s; cancel it first",
				owner.ID, owner.State)
		}
	}

	broken, err := st.ForceReleaseTFLock(ctx, ws.ID)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "workspace %s unlocked: lock %s, %s by %s since %s\n",
		ws.Name, broken.LockID, broken.Operation, broken.Who,
		broken.CreatedAt.Format("2006-01-02 15:04:05Z"))

	return auditUnlock(ctx, st, out, ws, broken)
}

// auditUnlock records the break on the job the lock named, falling back to the
// workspace's most recent job. A workspace that has never run a job has
// nothing to append to, and that is said out loud rather than passed over.
func auditUnlock(ctx context.Context, st *store.Store, out io.Writer,
	ws store.Workspace, broken store.TFLock) error {
	jobID, err := unlockAuditJob(ctx, st, ws, broken)
	if err != nil {
		return err
	}
	if jobID == 0 {
		_, err := fmt.Fprintf(out,
			"WARNING: workspace %s has no job to record the unlock on\n", ws.Name)
		return err
	}

	msg := fmt.Sprintf("force-unlocked tofu state: lock %s, %s by %s since %s",
		broken.LockID, broken.Operation, broken.Who,
		broken.CreatedAt.Format("2006-01-02 15:04:05Z"))
	if _, err := st.AppendJobEvent(ctx, store.JobEvent{
		JobID: jobID,
		Level: store.LevelSystem,
		Msg:   msg,
	}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "recorded on job %d\n", jobID)
	return err
}

// unlockAuditJob picks the job the audit row belongs on, returning 0 when the
// workspace has never had one.
func unlockAuditJob(ctx context.Context, st *store.Store, ws store.Workspace,
	broken store.TFLock) (int64, error) {
	if broken.JobID != nil {
		if _, err := st.GetJob(ctx, *broken.JobID); err == nil {
			return *broken.JobID, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return 0, err
		}
	}

	jobs, err := st.ListJobs(ctx, store.JobFilter{Runbook: workspaceRunbookPrefix + ws.Name})
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, job := range jobs {
		if job.ID > latest {
			latest = job.ID
		}
	}
	return latest, nil
}

func newWorkspaceAddCmd() *cobra.Command {
	var (
		dir       string
		stateOnly bool
		address   string
	)
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a tofu root module, or a state-only workspace",
		Long: "Registers a workspace exocomp runs (--dir), or, with --state-only, a " +
			"mode-A workspace exocomp only stores state for: it emits a ready-to-paste " +
			"backend \"http\" block plus its bearer so an unmodified external tofu keeps " +
			"its state encrypted and locked in exocomp while running plans itself.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if stateOnly {
				return runWorkspaceAddStateOnly(cmd, args[0], address)
			}
			if dir == "" {
				return errors.New("workspace add: --dir is required (or --state-only)")
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				return fmt.Errorf("workspace add: resolve %s: %w", dir, err)
			}
			info, err := os.Stat(abs)
			if err != nil {
				return fmt.Errorf("workspace add: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("workspace add: %s is not a directory", abs)
			}

			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			ws, err := st.CreateWorkspace(cmd.Context(), store.Workspace{
				Name:    args[0],
				TofuDir: abs,
			})
			if errors.Is(err, store.ErrDuplicate) {
				return fmt.Errorf("workspace add: %q already exists", args[0])
			}
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "workspace %d %s -> %s\n", ws.ID, ws.Name, ws.TofuDir)
			return nil
		},
	}
	add.Flags().StringVar(&dir, "dir", "", "path to the tofu root module (required unless --state-only)")
	add.Flags().BoolVar(&stateOnly, "state-only", false,
		"register a mode-A state-store-only workspace and emit its backend block")
	add.Flags().StringVar(&address, "address", "",
		"state backend address the emitted block targets (default: the recorded backend address)")
	return add
}

// runWorkspaceAddStateOnly registers a mode-A workspace (ADR 0010) and prints
// the backend "http" block an external tofu pastes. The bearer is deterministic
// (api.WorkspaceToken), so it is the same one the backend will check; it is
// printed once, kit discipline, and never stored beyond the workspace name it
// derives from.
func runWorkspaceAddStateOnly(cmd *cobra.Command, name, address string) (err error) {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("workspace add: resolve data dir: %w", err)
	}
	if address == "" {
		recorded, rerr := api.ReadAddress(filepath.Join(dir, api.AddressFile))
		if rerr != nil {
			return rerr
		}
		if recorded == "" {
			return errors.New("workspace add: no recorded state backend address; " +
				"pass --address host:port or start `exocomp serve` first")
		}
		address = recorded
	}

	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("workspace add: open envelope (run `exocomp init` first): %w", err)
	}
	defer func() { _ = env.Close() }()

	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	ws, err := st.CreateWorkspace(ctx, store.Workspace{Name: name})
	if errors.Is(err, store.ErrDuplicate) {
		// A state-only workspace already registered is fine to re-emit for: the
		// bearer is deterministic, so the operator just gets the block again.
		if existing, gerr := st.WorkspaceByName(ctx, name); gerr == nil {
			ws = existing
		} else {
			return fmt.Errorf("workspace add: %q already exists", name)
		}
	} else if err != nil {
		return err
	}

	token := api.WorkspaceToken(env, ws.Name)
	url := api.StateURL(address, ws.Name)
	return writeStateOnlyBackend(out, ws.Name, url, token)
}

// writeStateOnlyBackend prints the mode-A backend block. The address, lock and
// unlock URLs are one and the same (the SQLite backend serves LOCK/UNLOCK on
// the state path); the header carries the bearer inline so the block pastes as
// one unit.
func writeStateOnlyBackend(w io.Writer, name, url, token string) error {
	_, err := fmt.Fprintf(w, `# exocomp workspace %q — mode A (secure state store only).
# One exocomp workspace = one state = one backend URL. `+"`tofu workspace new`"+`
# and `+"`tofu workspace select`"+` are unsupported against this backend: use a
# separate exocomp workspace per environment. State lands encrypted and locked
# in exocomp; exocomp does not run your plan in this mode.
terraform {
  backend "http" {
    address        = %[2]q
    lock_address   = %[2]q
    unlock_address = %[2]q
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
    headers = {
      Authorization = "Bearer %[3]s"
    }
  }
}
`, name, url, token)
	return err
}

func newWorkspaceListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered workspaces",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			rows, err := st.ListWorkspaces(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no workspaces")
				return nil
			}
			for _, w := range rows {
				_, _ = fmt.Fprintf(out, "%-20s serial %-6d %s\n", w.Name, w.Serial, w.TofuDir)
			}
			return nil
		},
	}
}

func newWorkspacePlanCmd() *cobra.Command {
	var (
		timeout    time.Duration
		runSecrets string
	)
	plan := &cobra.Command{
		Use:   "plan <name>",
		Short: "Plan a workspace through the queue",
		Long: "Runs tofu plan against the workspace's state, saving the plan as an " +
			"artifact `workspace apply --plan` can consume. A running `exocomp serve` " +
			"runs the job while this command follows it. The plan runs with " +
			"-detailed-exitcode: this command exits 2 when changes are present, " +
			"while the job itself succeeds.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitWith(runWorkspaceOp(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				workspaceOp{workspace: args[0], op: "plan", runSecrets: runSecrets, timeout: timeout}))
		},
	}
	plan.Flags().DurationVar(&timeout, "timeout", 0, "kill the run after this long (0: no limit)")
	plan.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as provider/TF_VAR_ env for the run")
	return plan
}

func newWorkspaceApplyCmd() *cobra.Command {
	var (
		timeout    time.Duration
		planRef    string
		runSecrets string
	)
	apply := &cobra.Command{
		Use:   "apply <name>",
		Short: "Apply a workspace through the queue",
		Long: "Runs tofu apply against the workspace's state, through a running " +
			"`exocomp serve` when there is one. With --plan the run applies a plan a " +
			"previous job saved, and the apply job records the same artifact, which is " +
			"what links it to the plan that produced it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return exitWith(runWorkspaceOp(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				workspaceOp{workspace: args[0], op: "apply", planRef: planRef, runSecrets: runSecrets, timeout: timeout}))
		},
	}
	apply.Flags().DurationVar(&timeout, "timeout", 0, "kill the run after this long (0: no limit)")
	apply.Flags().StringVar(&planRef, "plan", "",
		"apply a saved plan, named by artifact id or by path")
	apply.Flags().StringVar(&runSecrets, "run-secrets", "",
		"bind a run_secret set as provider/TF_VAR_ env for the run")
	return apply
}

// exitWith turns a job's exit code into this process's. A cobra RunE cannot
// carry one, and the store and the envelope are already released by here.
func exitWith(code int, err error) error {
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

type workspaceOp struct {
	workspace  string
	op         string
	planRef    string
	runSecrets string
	timeout    time.Duration
}

// runWorkspaceOp runs one tofu operation through the queue and returns the
// exit code the shell should see. With a daemon up the job is enqueued for it
// and followed to its end; without one this process serves the state backend
// and drains the job itself, and the backend lives only as long as the run.
func runWorkspaceOp(ctx context.Context, out, warn io.Writer, op workspaceOp) (code int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	dir, err := dataDir()
	if err != nil {
		return 0, fmt.Errorf("workspace %s: resolve data dir: %w", op.op, err)
	}

	st, err := openJobStore(ctx, warn)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	ws, err := st.WorkspaceByName(ctx, op.workspace)
	if errors.Is(err, store.ErrNotFound) {
		return 0, fmt.Errorf("workspace %s: no workspace %q", op.op, op.workspace)
	}
	if err != nil {
		return 0, err
	}

	// Without the envelope there is no backend bearer and no state passphrase,
	// so there is no safe way to run at all.
	env, err := secrets.Open(dir)
	if err != nil {
		return 0, fmt.Errorf("workspace %s: open envelope (run `exocomp init` first): %w", op.op, err)
	}
	defer func() { _ = env.Close() }()

	dirs, err := tofuDirs(dir)
	if err != nil {
		return 0, err
	}

	argv, planPath, parent, cleanupPlan, err := opArgv(ctx, st, env, op, ws, dirs)
	if err != nil {
		return 0, err
	}
	defer cleanupPlan()
	spec := queue.Spec{
		Runbook:          workspaceRunbookPrefix + ws.Name,
		Kind:             executor.KindTofu,
		Trigger:          "manual",
		Argv:             argv,
		Dir:              ws.TofuDir,
		RunSecretBinding: op.runSecrets,
		ConcurrencyKey:   "tofu:" + ws.Name,
		Timeout:          op.timeout,
	}

	var final store.Job
	if daemonRunning(ctx, dir) {
		final, err = enqueueForDaemon(ctx, st, dir, spec, out)
	} else {
		final, err = runWorkspaceInline(ctx, st, env, dir, dirs, ws, spec, out, warn)
	}
	if err != nil {
		return 0, err
	}
	_, _ = fmt.Fprintf(out, "job %d  state: %s  duration: %s\n",
		final.ID, final.State, jobDuration(final))

	if parent != nil {
		if _, err := st.CreateArtifact(ctx, store.Artifact{
			JobID:  final.ID,
			Kind:   planArtifactKind,
			Path:   parent.Path,
			SHA256: parent.SHA256,
			Size:   parent.Size,
		}); err != nil {
			return 0, err
		}
		_, _ = fmt.Fprintf(out, "plan artifact %d, saved by job %d\n", parent.ID, parent.JobID)
	}
	if planPath != "" {
		if err := recordPlan(ctx, st, env, out, final.ID, planPath); err != nil {
			return 0, err
		}
	}

	return workspaceExit(ctx, st, out, final, op.op)
}

// runWorkspaceInline serves the state backend in this process and drains the
// one job it enqueued. The bearer and the state passphrase are derived here
// and never leave it, which is why a job row alone is not enough to run one.
func runWorkspaceInline(ctx context.Context, st *store.Store, env *secrets.Envelope, dir string,
	dirs tofuPaths, ws store.Workspace, spec queue.Spec, out, warn io.Writer) (job store.Job, err error) {
	addr, srv, err := stateBackend(ctx, dir, st, env)
	if err != nil {
		return store.Job{}, err
	}
	if srv != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
			defer cancel()
			if serr := srv.Shutdown(shutdownCtx); serr != nil && err == nil {
				err = serr
			}
		}()
	}

	token := api.WorkspaceToken(env, ws.Name)
	stateKey := env.Derive(executor.TofuStatePurpose, ws.Name)
	passphrase := base64.StdEncoding.EncodeToString(stateKey)

	runSecrets, secretPlain, err := resolveRunSecrets(ctx, st, env, spec.RunSecretBinding, "")
	if err != nil {
		return store.Job{}, err
	}

	known := knownSecrets(ctx, st, env, warn)
	known = append(known, []byte(token), stateKey, []byte(passphrase))
	known = append(known, secretPlain...)

	driver := tofuTierDriver(dirs.root, executor.TofuOptions{
		Workspace:       ws.Name,
		BackendURL:      api.StateURL(addr, ws.Name),
		BackendToken:    token,
		StatePassphrase: passphrase,
		PrivateDirRoot:  dirs.private,
		PluginCacheDir:  dirs.pluginCache,
		RunSecrets:      runSecrets,
	})
	q := queue.New(st, driver, queue.Options{
		Workers:      1,
		PollInterval: 50 * time.Millisecond,
	}, queue.WithRedactor(secrets.NewRedactor(known)))

	queued, err := q.Enqueue(ctx, spec)
	if err != nil {
		return store.Job{}, err
	}
	if srv != nil {
		srv.BindJob(ws.Name, queued.ID)
		defer srv.ReleaseJob(ws.Name)
	}
	if err := drainAndTail(ctx, q, st, queued.ID, out); err != nil {
		return store.Job{}, err
	}
	return st.GetJob(ctx, queued.ID)
}

// stateBackend resolves the address tofu talks to, returning a server only
// when this process had to start one.
//
// A saved plan carries the backend address it was made against, so a plan and
// the apply that consumes it must find the same one. The address recorded in
// the data dir is that agreement: a live server there is reused, and a stale
// entry is re-bound so a plan made by the previous command still applies.
func stateBackend(ctx context.Context, dir string, st *store.Store, env *secrets.Envelope) (
	string, *api.Server, error) {
	path := filepath.Join(dir, api.AddressFile)
	recorded, err := api.ReadAddress(path)
	if err != nil {
		return "", nil, err
	}
	if api.Reachable(ctx, recorded) {
		return recorded, nil, nil
	}

	srv, err := startBackend(st, env, api.PortOf(recorded))
	if err != nil {
		return "", nil, err
	}
	if err := api.WriteAddress(path, srv.Addr()); err != nil {
		return "", nil, err
	}
	return srv.Addr(), srv, nil
}

// startBackend binds the remembered port, falling back to an ephemeral one
// when something that is not a state backend already holds it.
func startBackend(st *store.Store, env *secrets.Envelope, port int) (*api.Server, error) {
	if port != 0 {
		srv, err := api.New(api.Options{Store: st, Tokens: env, Port: port})
		if err != nil {
			return nil, err
		}
		if err := srv.Start(); err == nil {
			return srv, nil
		}
	}

	srv, err := api.New(api.Options{Store: st, Tokens: env, Port: 0})
	if err != nil {
		return nil, err
	}
	if err := srv.Start(); err != nil {
		return nil, err
	}
	return srv, nil
}

// opArgv builds the tofu argv for the operation and reports the plan file it
// will write and the saved plan it will consume.
func opArgv(ctx context.Context, st *store.Store, env *secrets.Envelope, op workspaceOp,
	ws store.Workspace, dirs tofuPaths) (
	argv []string, planPath string, parent *store.Artifact, cleanup func(), err error) {
	cleanup = func() {}
	switch op.op {
	case "plan":
		planPath = filepath.Join(dirs.artifacts,
			fmt.Sprintf("%s-%d.tfplan", ws.Name, time.Now().UTC().UnixNano()))
		return executor.TofuArgv("plan", "-out="+planPath), planPath, nil, cleanup, nil
	case "apply":
		if op.planRef == "" {
			return executor.TofuArgv("apply", "-auto-approve"), "", nil, cleanup, nil
		}
		saved, err := resolvePlan(ctx, st, op.planRef)
		if err != nil {
			return nil, "", nil, cleanup, err
		}
		// The stored plan is ciphertext; tofu reads the plaintext copy this
		// run makes and the cleanup removes.
		readable, release, err := openPlanForRun(env, dirs.private, saved.Path)
		if err != nil {
			return nil, "", nil, cleanup, err
		}
		return executor.TofuArgv("apply", readable), "", &saved, release, nil
	default:
		return nil, "", nil, cleanup, fmt.Errorf("workspace: unknown operation %q", op.op)
	}
}

// resolvePlan accepts either the artifact id a previous plan printed or a path
// to the plan file itself.
func resolvePlan(ctx context.Context, st *store.Store, ref string) (store.Artifact, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		saved, err := st.GetArtifact(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return store.Artifact{}, fmt.Errorf("workspace apply: no artifact %d", id)
		}
		if err != nil {
			return store.Artifact{}, err
		}
		if saved.Kind != planArtifactKind {
			return store.Artifact{}, fmt.Errorf("workspace apply: artifact %d is a %s, not a plan",
				id, saved.Kind)
		}
		return saved, nil
	}

	abs, err := filepath.Abs(ref)
	if err != nil {
		return store.Artifact{}, fmt.Errorf("workspace apply: resolve %s: %w", ref, err)
	}
	sum, size, err := fileDigest(abs)
	if err != nil {
		return store.Artifact{}, fmt.Errorf("workspace apply: %w", err)
	}
	return store.Artifact{Kind: planArtifactKind, Path: abs, SHA256: sum, Size: size}, nil
}

// recordPlan registers the plan file a run wrote, encrypted at rest first. A
// run that changed nothing still writes one, so its absence means the plan
// never got that far. The digest recorded is of the ciphertext, because that
// is what is on disk to check later.
func recordPlan(ctx context.Context, st *store.Store, env *secrets.Envelope, out io.Writer,
	jobID int64, path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	path, err := sealArtifact(env, planArtifactKind, path)
	if err != nil {
		return err
	}
	sum, size, err := fileDigest(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	saved, err := st.CreateArtifact(ctx, store.Artifact{
		JobID:  jobID,
		Kind:   planArtifactKind,
		Path:   path,
		SHA256: sum,
		Size:   size,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "saved plan artifact %d at %s\n", saved.ID, saved.Path)
	return nil
}

// workspaceExit reports the shell exit code, translating a plan that found
// changes into 2 the way `tofu plan -detailed-exitcode` does.
func workspaceExit(ctx context.Context, st *store.Store, out io.Writer, job store.Job, op string) (int, error) {
	if op == "plan" && job.State == store.StateSucceeded {
		changes, err := planChanges(ctx, st, job.ID)
		if err != nil {
			return 0, err
		}
		if changes {
			_, _ = fmt.Fprintln(out, "plan: changes present")
			return planExitChanges, nil
		}
		_, _ = fmt.Fprintln(out, "plan: no changes")
		return 0, nil
	}

	switch {
	case job.State == store.StateSucceeded:
		return 0, nil
	case job.ExitCode != nil && *job.ExitCode != 0:
		return *job.ExitCode, nil
	default:
		return 1, nil
	}
}

// planChanges reads the plan outcome back out of the job's own events, which
// is where the driver put it: exit code 2 is neither success nor failure and
// has nowhere else durable to live.
func planChanges(ctx context.Context, st *store.Store, jobID int64) (bool, error) {
	events, err := st.ListJobEvents(ctx, jobID)
	if err != nil {
		return false, err
	}
	for _, e := range events {
		if changes, ok := executor.TofuPlanChanges(e.RawJSON); ok {
			return changes, nil
		}
	}
	return false, nil
}

// tofuPaths are the per-installation directories a tofu run needs. All of them
// sit inside the 0700 data dir: plan files hold resource values in the clear
// and the private root holds the generated backend bearer.
type tofuPaths struct {
	root        string
	artifacts   string
	private     string
	pluginCache string
}

func tofuDirs(dataDir string) (tofuPaths, error) {
	p := tofuPaths{
		root:        dataDir,
		artifacts:   filepath.Join(dataDir, "artifacts", "tofu"),
		private:     filepath.Join(dataDir, privateRunDir),
		pluginCache: filepath.Join(dataDir, "tofu", "plugin-cache"),
	}
	for _, d := range []string{p.artifacts, p.private, p.pluginCache} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return tofuPaths{}, fmt.Errorf("workspace: create %s: %w", d, err)
		}
	}
	return p, nil
}

func fileDigest(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	size, err = io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("digest %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
