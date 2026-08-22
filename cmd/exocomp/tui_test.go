package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRunTUIRejectsNonTerminalStdin(t *testing.T) {
	useDataDir(t, t.TempDir())

	cmd := &cobra.Command{}
	cmd.SetIn(bytes.NewReader(nil))

	err := runTUI(cmd)
	if err == nil {
		t.Fatal("runTUI with a non-terminal stdin returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "not a terminal") {
		t.Fatalf("error = %q, want a clean non-terminal message", err)
	}
	if strings.Contains(err.Error(), "epoll") || strings.Contains(err.Error(), "cancelable reader") {
		t.Fatalf("error = %q, leaked the raw bubbletea library error", err)
	}
}
