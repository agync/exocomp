// Command exocomp is the exocompd control-plane CLI.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/version"
)

// commandFactories collects per-file subcommand constructors so parallel
// lanes register commands without editing shared files.
var commandFactories []func() *cobra.Command

func registerCommand(f func() *cobra.Command) {
	commandFactories = append(commandFactories, f)
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "exocomp",
		Short:         "exocomp homelab control plane",
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.AddCommand(newVersionCmd())
	root.AddCommand(newDoctorCmd())
	for _, f := range commandFactories {
		root.AddCommand(f())
	}

	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, and build date",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return err
		},
	}
}
