package main

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/secrets"
)

func init() {
	registerCommand(newServeCmd)
}

// shutdownGrace bounds how long a stopping server waits for the request it is
// already serving. A state push is one INSERT; anything slower is stuck.
const shutdownGrace = 5 * time.Second

func newServeCmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the exocompd service",
		Long: "Serves the OpenTofu HTTP state backend on 127.0.0.1 and exits cleanly " +
			"on SIGINT or SIGTERM. Each workspace is authenticated by a bearer " +
			"derived from the envelope, so the port carries no authority of its own " +
			"beyond what a local process could already derive.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), port)
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "loopback port to bind (0: ephemeral)")
	return cmd
}

func runServe(ctx context.Context, out, warn io.Writer, port int) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("serve: resolve data dir: %w", err)
	}

	st, err := openJobStore(ctx, warn)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("serve: open envelope (run `exocomp init` first): %w", err)
	}
	defer func() { _ = env.Close() }()

	srv, err := api.New(api.Options{Store: st, Tokens: env, Port: port})
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	if err := api.WriteAddress(filepath.Join(dir, api.AddressFile), srv.Addr()); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "state backend listening on http://%s\n", srv.Addr())

	<-ctx.Done()
	_, _ = fmt.Fprintln(out, "shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
