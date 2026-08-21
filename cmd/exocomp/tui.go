package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/tui"
)

func init() {
	registerCommand(newTUICmd)
}

func newTUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the terminal UI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTUI(cmd)
		},
	}
}

func runTUI(cmd *cobra.Command) (err error) {
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("tui: resolve data dir: %w", err)
	}

	dbPath := filepath.Join(dir, "exocomp.db")
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("tui: no database at %s; run `exocomp init` or `exocomp devrun` first", dbPath)
		}
		return fmt.Errorf("tui: stat %s: %w", dbPath, statErr)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("tui: open store: %w", err)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	p := tea.NewProgram(tui.New(st),
		tea.WithContext(cmd.Context()),
		tea.WithOutput(cmd.OutOrStdout()),
		tea.WithInput(cmd.InOrStdin()),
	)
	_, err = p.Run()
	return err
}
