package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/wire"
)

const (
	outputText = "text"
	outputJSON = "json"
	outputFlag = "output"
	outputEnv  = "EXOCOMP_OUTPUT"
)

// addOutputFlag registers the global --output format flag on the root command.
// It is the one place the flag is declared; read/list/status verbs read it back
// through outputMode.
func addOutputFlag(root *cobra.Command) {
	root.PersistentFlags().String(outputFlag, outputText,
		"output format: text|json (or EXOCOMP_OUTPUT)")
}

// outputMode resolves the effective format: an explicit --output flag wins, then
// EXOCOMP_OUTPUT, then the text default. It tolerates the flag being absent so a
// subcommand executed in isolation (as the unit tests do) still resolves.
func outputMode(cmd *cobra.Command) (string, error) {
	mode := outputText
	if f := cmd.Flags().Lookup(outputFlag); f != nil && f.Changed {
		mode = f.Value.String()
	} else if env := strings.TrimSpace(os.Getenv(outputEnv)); env != "" {
		mode = env
	}
	switch mode {
	case outputText, outputJSON:
		return mode, nil
	default:
		return "", fmt.Errorf("--output must be %s or %s, not %q", outputText, outputJSON, mode)
	}
}

// emitJSON writes a wire success envelope to the command's stdout.
func emitJSON(cmd *cobra.Command, kind string, data any) error {
	return wire.WriteSuccess(cmd.OutOrStdout(), kind, data)
}

// failJSON writes err as a wire error envelope to stdout and silences cobra's
// own stderr print, while still returning err so the process exits nonzero. It
// is used only on the JSON path; the text path keeps its existing prose errors.
func failJSON(cmd *cobra.Command, err error) error {
	cmd.SilenceErrors = true
	_ = wire.WriteError(cmd.OutOrStdout(), err)
	return err
}
