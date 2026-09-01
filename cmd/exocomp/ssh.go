package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	wishtea "charm.land/wish/v2/bubbletea"
	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/tui"
)

// hostKeyName is the private host key file, written 0600 into the data dir
// the first time ssh-serve runs. It is ed25519 like every other key this
// project mints; keygen writes it and its .pub sibling.
const hostKeyName = "ssh_host_ed25519"

// shutdownTimeout bounds how long a graceful shutdown waits for open
// sessions before the command returns anyway.
const shutdownTimeout = 5 * time.Second

func init() {
	registerCommand(newSSHServeCmd)
}

func newSSHServeCmd() *cobra.Command {
	var listen, authorizedKeys string

	cmd := &cobra.Command{
		Use:   "ssh-serve",
		Short: "Serve the TUI over SSH, authenticated by an authorized_keys file",
		Long: "Public-key auth only — there is no password path. Every accepted session " +
			"gets its own TUI over the same store; the server exits when its context is " +
			"cancelled.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSSHServe(cmd, listen, authorizedKeys)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:2222", "address to listen on")
	cmd.Flags().StringVar(&authorizedKeys, "authorized-keys", "", "path to an authorized_keys file (required)")
	_ = cmd.MarkFlagRequired("authorized-keys")
	return cmd
}

func runSSHServe(cmd *cobra.Command, listen, authorizedKeys string) error {
	if _, err := os.Stat(authorizedKeys); err != nil {
		return fmt.Errorf("ssh-serve: authorized keys: %w", err)
	}

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("ssh-serve: resolve data dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ssh-serve: create data dir: %w", err)
	}

	st, err := openJobStore(cmd.Context(), cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("ssh-serve: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	srv, err := wish.NewServer(
		wish.WithAddress(listen),
		wish.WithHostKeyPath(filepath.Join(dir, hostKeyName)),
		wish.WithAuthorizedKeys(authorizedKeys),
		wish.WithMiddleware(
			wishtea.Middleware(func(_ ssh.Session) (tea.Model, []tea.ProgramOption) {
				return tui.New(st, &tui.Write{
					OpenEnvelope: func() (*secrets.Envelope, error) { return secrets.Open(dir) },
					DataDir:      dir,
				}), nil
			}),
		),
	)
	if err != nil {
		return fmt.Errorf("ssh-serve: create server: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "listening on %s (authorized keys: %s)\n", listen, authorizedKeys)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case <-cmd.Context().Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("ssh-serve: shutdown: %w", err)
		}
		return nil
	case err := <-errc:
		if errors.Is(err, ssh.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("ssh-serve: %w", err)
	}
}
