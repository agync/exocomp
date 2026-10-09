package main

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// redriveTrigger labels a job the operator re-drove after a restart or a crash
// cut its first run off, so the job stream shows why this run exists. It
// mirrors the web "Run again" rerun trigger, under its own word: a redrive is
// a bulk recovery of cut-off work, not a one-off resubmit from a detail page.
const redriveTrigger = "redrive"

// redriveKind is the JSON envelope kind for a redrive report. It is defined
// here, next to the verb that emits it, rather than in internal/wire: moving
// it into the wire kind registry is follow-up work if a second face ever needs
// the same shape.
const redriveKind = "job.redrive"

func newJobsRedriveCmd() *cobra.Command {
	var (
		states []string
		limit  int
		dryRun bool
		yes    bool
	)
	cmd := &cobra.Command{
		Use:   "redrive",
		Short: "Re-drive jobs a restart or crash cut off",
		Long: "Re-enqueues the jobs a forced restart or a crash cut off — the " +
			"interrupted and cancelled rows nothing auto-retries — as fresh queued " +
			"rows carrying the same spec, for the running daemon to claim. " +
			"Each new job is labelled with the redrive trigger.\n\n" +
			"A real run is a bulk re-run of possibly destructive work, so it " +
			"needs confirmation: pass --yes (or the global --assume-yes), or " +
			"answer the prompt on an interactive terminal. --dry-run prints " +
			"what would be re-driven and enqueues nothing, and needs no " +
			"confirmation. JSON mode never prompts: pass --yes.\n\n" +
			"Redrive always acts on this box's store; it takes no --remote.",
		Example: "exocomp jobs redrive --dry-run\nexocomp jobs redrive --yes\nexocomp jobs redrive --state cancelled --yes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJobsRedrive(cmd, redriveOptions{states: states, limit: limit, dryRun: dryRun, yes: yes})
		},
	}
	cmd.Flags().StringSliceVar(&states, "state", []string{string(store.StateInterrupted)},
		"only jobs in this state (repeatable: interrupted, cancelled)")
	cmd.Flags().IntVar(&limit, "limit", 0, "how many jobs to re-drive (0: all)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be re-driven and enqueue nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm this bulk re-run (the global --assume-yes also confirms)")
	return cmd
}

type redriveOptions struct {
	states []string
	limit  int
	dryRun bool
	yes    bool
}

// redrivePair is one re-driven job: the cut-off row and the fresh queued row.
type redrivePair struct {
	old store.Job
	new store.Job
}

// redriveSkip is a selected row that cannot be re-driven, with the reason.
type redriveSkip struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// redriveItem is the JSON projection of one re-driven job.
type redriveItem struct {
	From    int64  `json:"from"`
	To      *int64 `json:"to,omitempty"`
	Runbook string `json:"runbook"`
	Kind    string `json:"kind"`
}

// redriveReport is the JSON payload of a redrive run. Redriven is never nil,
// so an empty selection renders [] rather than null.
type redriveReport struct {
	DryRun   bool          `json:"dry_run"`
	Redriven []redriveItem `json:"redriven"`
	Skipped  []redriveSkip `json:"skipped,omitempty"`
}

// runJobsRedrive selects the cut-off rows, confirms the bulk re-run, and
// re-enqueues each through service.Reenqueue — the same primitive the web "Run
// again" uses — so there is exactly one re-enqueue path.
func runJobsRedrive(cmd *cobra.Command, opt redriveOptions) error {
	mode, err := outputMode(cmd)
	if err != nil {
		return err
	}
	if opt.limit < 0 {
		return errors.New("jobs redrive: --limit cannot be negative")
	}
	states := opt.states
	if len(states) == 0 {
		states = []string{string(store.StateInterrupted)}
	}
	filter := store.JobFilter{Limit: opt.limit}
	for _, s := range states {
		if !knownJobState(s) {
			return fmt.Errorf("jobs redrive: --state %q is not a job state", s)
		}
		if store.JobState(s) != store.StateInterrupted && store.JobState(s) != store.StateCancelled {
			return fmt.Errorf("jobs redrive: --state %q cannot be re-driven (only interrupted, cancelled)", s)
		}
		filter.States = append(filter.States, store.JobState(s))
	}

	// The sweep-coupled opener, like every other job path: rows a dead process
	// left stranded are retired to interrupted first, which is exactly what
	// makes a post-crash redrive see them. Sweep notices go to stderr, so the
	// JSON report on stdout stays one parseable document.
	st, err := openJobStore(cmd.Context(), cmd.ErrOrStderr())
	if err != nil {
		if mode == outputJSON {
			return failJSON(cmd, err)
		}
		return err
	}
	defer func() { _ = st.Close() }()

	rows, err := st.ListJobs(cmd.Context(), filter)
	if err != nil {
		if mode == outputJSON {
			return failJSON(cmd, err)
		}
		return err
	}

	var (
		candidates []store.Job
		skipped    []redriveSkip
	)
	for _, job := range rows {
		if len(job.Args.Argv) == 0 {
			skipped = append(skipped, redriveSkip{ID: job.ID, Reason: "no command to re-drive"})
			continue
		}
		candidates = append(candidates, job)
	}

	if len(candidates) == 0 && len(skipped) == 0 {
		return renderRedriveEmpty(cmd, mode)
	}

	if opt.dryRun {
		return renderRedrive(cmd, mode, nil, candidates, skipped, true)
	}

	if err := confirmRedrive(cmd, mode, len(candidates), opt.yes); err != nil {
		if mode == outputJSON {
			return failJSON(cmd, err)
		}
		return err
	}

	dir, err := dataDir()
	if err != nil {
		if mode == outputJSON {
			return failJSON(cmd, err)
		}
		return err
	}
	q := queue.New(st, enqueueOnly{}, queue.Options{Workers: 1})
	svc := service.New(st, dir, nil)
	pairs := make([]redrivePair, 0, len(candidates))
	for _, job := range candidates {
		res, err := svc.Reenqueue(cmd.Context(), q, redriveSpec(job), service.Actor{})
		if err != nil {
			if mode == outputJSON {
				return failJSON(cmd, err)
			}
			return fmt.Errorf("jobs redrive: re-drive job %d: %w", job.ID, err)
		}
		pairs = append(pairs, redrivePair{old: job, new: res.Job})
	}
	return renderRedrive(cmd, mode, pairs, nil, skipped, false)
}

// redriveSpec rebuilds the run's spec from the cut-off row's stored args: every
// run needs already lives on the row, secret material by name only, so the
// fresh job replays the same run. The trigger is redrive, not a copy of the
// original, so the new run says why it exists. The idempotency and dedup keys
// are deliberately dropped: carrying the idempotency key would collapse the
// enqueue back onto the original row instead of landing a fresh job.
func redriveSpec(job store.Job) queue.Spec {
	return queue.Spec{
		Runbook:             job.Runbook,
		Kind:                job.Kind,
		Trigger:             redriveTrigger,
		Argv:                append([]string(nil), job.Args.Argv...),
		Dir:                 job.Args.Dir,
		DirReadOnly:         job.Args.DirReadOnly,
		VaultProject:        job.Args.VaultProject,
		RunSecretBinding:    job.Args.RunSecretBinding,
		Inventory:           job.Args.Inventory,
		Source:              job.Args.Source,
		ExtraVars:           job.Args.ExtraVars,
		Captures:            job.Args.Captures,
		RunnerName:          job.Args.RunnerName,
		TolerateUnreachable: job.Args.TolerateUnreachable,
		Timeout:             time.Duration(job.Args.Timeout),
		Privileged:          job.Args.Privileged,
		EgressPorts:         job.Args.EgressPorts,
		SSHIdentityFiles:    job.Args.SSHIdentityFiles,
		ConcurrencyKey:      job.ConcurrencyKey,
	}
}

// confirmRedrive gates the bulk re-run behind an explicit confirmation. --yes
// or the global --assume-yes proceeds at once; otherwise an interactive
// terminal is asked, and anything else — --no-input, a pipe, JSON mode whose
// stdout a script parses — is refused with the flag to pass. Stdin is never
// read except for the TTY prompt, so a non-interactive session cannot hang.
func confirmRedrive(cmd *cobra.Command, mode string, n int, yes bool) error {
	if yes || boolFlag(cmd, assumeYesFlag) {
		return nil
	}
	if mode == outputJSON {
		return errors.New("jobs redrive: re-driving jobs needs confirmation and --output json never prompts: pass --yes")
	}
	if boolFlag(cmd, noInputFlag) || !stdinIsInteractive(cmd) {
		return errors.New("jobs redrive: re-driving jobs needs confirmation and stdin is non-interactive: pass --yes")
	}
	return promptRedrive(cmd, n)
}

// promptRedrive asks the operator once on their terminal. Only an explicit
// yes proceeds; anything else aborts the run and enqueues nothing.
func promptRedrive(cmd *cobra.Command, n int) error {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "re-drive %d job(s)? [y/N] ", n)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil {
		return errors.New("jobs redrive: not confirmed")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return errors.New("jobs redrive: not confirmed")
	}
}

// renderRedriveEmpty reports a selection that matched nothing.
func renderRedriveEmpty(cmd *cobra.Command, mode string) error {
	if mode == outputJSON {
		return emitJSON(cmd, redriveKind, redriveReport{DryRun: false, Redriven: []redriveItem{}})
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), "no jobs to redrive")
	return err
}

// renderRedrive reports a dry run (would) or a real run (pairs). would holds
// the rows a dry run would re-drive; pairs holds a real run's old-to-new rows.
func renderRedrive(cmd *cobra.Command, mode string, pairs []redrivePair, would []store.Job, skipped []redriveSkip, dryRun bool) error {
	if mode == outputJSON {
		return emitJSON(cmd, redriveKind, redriveJSON(pairs, would, skipped, dryRun))
	}
	return renderRedriveText(cmd, pairs, would, skipped, dryRun)
}

// redriveJSON builds the report payload: on a dry run each item carries its
// source id and no new id; on a real run each carries the old-to-new mapping.
func redriveJSON(pairs []redrivePair, would []store.Job, skipped []redriveSkip, dryRun bool) redriveReport {
	items := []redriveItem{}
	if dryRun {
		for _, job := range would {
			items = append(items, redriveItem{From: job.ID, Runbook: job.Runbook, Kind: job.Kind})
		}
	} else {
		for _, p := range pairs {
			to := p.new.ID
			items = append(items, redriveItem{From: p.old.ID, To: &to, Runbook: p.old.Runbook, Kind: p.old.Kind})
		}
	}
	return redriveReport{DryRun: dryRun, Redriven: items, Skipped: skipped}
}

// renderRedriveText prints each old-to-new id pair and a summary, naming the
// rows a dry run would touch and the rows skipped for lack of a command.
func renderRedriveText(cmd *cobra.Command, pairs []redrivePair, would []store.Job, skipped []redriveSkip, dryRun bool) error {
	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if dryRun {
		for _, job := range would {
			_, _ = fmt.Fprintf(w, "would re-drive #%d\t%s\t%s\t%s\n",
				job.ID, dashIfEmpty(job.Runbook), dashIfEmpty(job.Kind), dashIfEmpty(job.Trigger))
		}
	} else {
		for _, p := range pairs {
			_, _ = fmt.Fprintf(w, "#%d -> #%d\t%s\t%s\n",
				p.old.ID, p.new.ID, dashIfEmpty(p.old.Runbook), dashIfEmpty(p.old.Kind))
		}
	}
	for _, s := range skipped {
		_, _ = fmt.Fprintf(w, "skipped #%d\t%s\n", s.ID, s.Reason)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	switch {
	case dryRun:
		_, _ = fmt.Fprintf(out, "would re-drive %d job(s); nothing enqueued (--dry-run)\n", len(would))
	case len(pairs) == 0:
		_, _ = fmt.Fprintln(out, "nothing re-driven")
	default:
		_, _ = fmt.Fprintf(out, "redrove %d job(s)\n", len(pairs))
		if dir, err := dataDir(); err == nil && !daemonRunning(cmd.Context(), dir) {
			_, _ = fmt.Fprintln(out, "no daemon running; the new jobs stay queued until one claims them")
		}
	}
	return nil
}
