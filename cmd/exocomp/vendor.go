package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/deps"
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

// galaxyRequirementsHash is the content hash a requirements file's per-source
// collections dir is keyed by. It must match internal/deps' hashGalaxy (sha256,
// first 32 hex chars) so `playbook vendor` lands exactly where a later run's
// resolve (deps.DiscoverGalaxy → EnsureCollections) looks. vendor hashes the
// named file itself because --requirements may name any file, not only the
// manifest a resolve would discover.
func galaxyRequirementsHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:32]
}

func newPlaybookVendorCmd() *cobra.Command {
	var requirements string

	cmd := &cobra.Command{
		Use:   "vendor",
		Short: "Install ansible collections into the offline cache",
		Long: "Runs `ansible-galaxy collection install -r <requirements>` into " +
			"the per-source cache dir <data dir>/cache/collections/<source>/<hash> " +
			"through the queue, so a later playbook run resolves those collections " +
			"without Ansible Galaxy being reachable (Decision 0015). <source> and <hash> " +
			"derive from the --requirements file's project root and content, the " +
			"same dir the run path resolves. Needs network access for whatever the " +
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
	// The install target is per-source (Decision 0015): keyed by the requirements
	// file's project root and content hash, not the shared cache dir, so the
	// vendor step and the run path's resolve land in the same dir. The shared
	// dir is left as a read-only fallback nothing writes anymore.
	projectRoot := deps.FindProjectRoot(filepath.Dir(requirements))
	source := deps.SourceKey(projectRoot)
	body, err := os.ReadFile(requirements)
	if err != nil {
		return 0, fmt.Errorf("playbook vendor: read requirements: %w", err)
	}
	hash := galaxyRequirementsHash(body)
	cache := filepath.Join(dir, collectionsCacheDir, source, hash)
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
		// A run path's EnsureCollections reuses a dir only when its ready
		// marker carries the requirements hash; without one it treats the dir
		// as half-built, removes it, and re-dials galaxy. Mark the dir the
		// install just populated so the vendored collections serve the next
		// run offline (Decision 0015).
		if err := deps.MarkCollectionsReady(cache, hash); err != nil {
			return 0, fmt.Errorf("playbook vendor: mark collections ready: %w", err)
		}
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
