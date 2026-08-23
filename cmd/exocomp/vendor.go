package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// collectionsCacheDir is where vendored ansible collections live inside the
// data dir. The ansible driver appends it to ANSIBLE_COLLECTIONS_PATH
// (design/amendments.md item 8), so a playbook resolves them without
// Ansible Galaxy being reachable.
const collectionsCacheDir = "cache/collections"

// vendorKind is the queue kind this command's job runs as. It is a plain
// shell job, not KindAnsible: ansible-galaxy is a different binary with a
// different argv shape than a playbook run.
const vendorKind = "shell"

func newPlaybookVendorCmd() *cobra.Command {
	var requirements string

	cmd := &cobra.Command{
		Use:   "vendor",
		Short: "Install ansible collections into the offline cache",
		Long: "Runs `ansible-galaxy collection install -r <requirements>` into " +
			"<data dir>/cache/collections through the queue, so a later playbook run " +
			"resolves those collections without Ansible Galaxy being reachable " +
			"(design/amendments.md item 8). Needs network access for whatever the " +
			"cache does not already hold.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if requirements == "" {
				return errors.New("playbook vendor: --requirements is required")
			}
			req, err := filepath.Abs(requirements)
			if err != nil {
				return fmt.Errorf("playbook vendor: resolve %s: %w", requirements, err)
			}
			code, err := runVendor(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), req, idempotencyKey(cmd))
			if err != nil {
				return err
			}
			if code != 0 {
				// A cobra RunE cannot carry an exit code; runVendor has
				// already released the store by here.
				os.Exit(code)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&requirements, "requirements", "", "ansible-galaxy requirements.yml (required)")
	_ = cmd.MarkFlagRequired("requirements")
	return cmd
}

// runVendor returns the job's exit code, and an error only when the job
// could not be run at all.
func runVendor(ctx context.Context, out, warn io.Writer, requirements, idemKey string) (code int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	dir, err := dataDir()
	if err != nil {
		return 0, fmt.Errorf("playbook vendor: resolve data dir: %w", err)
	}
	cache := filepath.Join(dir, collectionsCacheDir)
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return 0, fmt.Errorf("playbook vendor: create cache dir: %w", err)
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

	spec := queue.Spec{
		Runbook:        "playbook-vendor",
		Kind:           vendorKind,
		Trigger:        "manual",
		Argv:           []string{"ansible-galaxy", "collection", "install", "-r", requirements, "-p", cache},
		IdempotencyKey: idemKey,
	}

	var final store.Job
	if daemonRunning(ctx, dir) {
		final, err = enqueueForDaemon(ctx, st, dir, spec, out)
	} else {
		final, err = runVendorInline(ctx, st, spec, out)
	}
	if err != nil {
		return 0, err
	}
	_, _ = fmt.Fprintf(out, "job %d  state: %s  duration: %s\n",
		final.ID, final.State, jobDuration(final))

	switch {
	case final.State == store.StateSucceeded:
		return 0, nil
	case final.ExitCode != nil && *final.ExitCode != 0:
		return *final.ExitCode, nil
	default:
		return 1, nil
	}
}

// runVendorInline drains the one job in this process, which is what a host
// with no daemon does.
func runVendorInline(ctx context.Context, st *store.Store, spec queue.Spec, out io.Writer) (store.Job, error) {
	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      1,
		PollInterval: 50 * time.Millisecond,
	})
	job, err := q.Enqueue(ctx, spec)
	if err != nil {
		return store.Job{}, err
	}
	if err := drainAndTail(ctx, q, st, job.ID, out); err != nil {
		return store.Job{}, err
	}
	return st.GetJob(ctx, job.ID)
}
