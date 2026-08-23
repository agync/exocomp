package main

import (
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/mcp"
)

func init() {
	registerCommand(newMCPCmd)
}

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "mcp",
		Short:   "Serve the exocomp MCP server over stdio",
		Example: "exocomp mcp",
		Long: "Speaks the Model Context Protocol over stdin/stdout so an agent harness " +
			"drives exocomp as tools and resources. Every tool and resource maps to the " +
			"same internal service verbs and store reads the CLI and the HTTP API use, and " +
			"emits the same values-free wire envelope. Writes enqueue through the one queue; " +
			"a running `exocomp serve` claims and runs the queued row. Stdio runs at the " +
			"local operator's authority (this process holds the data dir); a token-scoped " +
			"HTTP/SSE transport is a follow-up. Because stdio carries no credential, it " +
			"offers NO approval decision: a job this face gates is released from the " +
			"browser/phone surface, the CLI, or an approve-scope /api/v1 token.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			dir, err := dataDir()
			if err != nil {
				return err
			}
			st, err := openStoreAt(dir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			srv := mcp.New(st, dir, nil)
			// The protocol owns stdout; diagnostics must not corrupt the frames, so
			// the logger writes to stderr.
			srv.SetLogger(slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)))
			return srv.Serve(ctx, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
