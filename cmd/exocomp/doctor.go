package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// optionalTool is a tool exocompd shells out to that is not required in M0.
type optionalTool struct {
	name string
	args []string
}

var optionalTools = []optionalTool{
	{name: "ansible-playbook", args: []string{"--version"}},
	{name: "tofu", args: []string{"version"}},
	{name: "restic", args: []string{"version"}},
	{name: "age", args: []string{"--version"}},
	{name: "sops", args: []string{"--version"}},
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run environment checks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.OutOrStdout())
		},
	}
}

func runDoctor(w io.Writer) error {
	p := func(format string, a ...any) {
		_, _ = fmt.Fprintf(w, format, a...)
	}
	p("go runtime:   %s\n", runtime.Version())

	dataDir, err := dataDir()
	if err != nil {
		return fmt.Errorf("doctor: resolve data dir: %w", err)
	}
	if err := checkDataDirWritable(dataDir); err != nil {
		p("data dir:     %s — NOT WRITABLE (%v)\n", dataDir, err)
		return fmt.Errorf("doctor: data dir not writable: %w", err)
	}
	p("data dir:     %s — writable\n", dataDir)

	p("optional tools (M0 probe subset):\n")
	for _, t := range optionalTools {
		p("  %-18s %s\n", t.name, probeTool(t))
	}

	return nil
}

func dataDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "exocomp"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "exocomp"), nil
}

func checkDataDirWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".doctor-probe-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

func probeTool(t optionalTool) string {
	path, err := exec.LookPath(t.name)
	if err != nil {
		return "MISSING"
	}

	out, err := exec.Command(path, t.args...).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Sprintf("found at %s (version probe exited %d)", path, exitErr.ExitCode())
		}
		return fmt.Sprintf("found at %s (version probe failed: %v)", path, err)
	}

	return fmt.Sprintf("found at %s (%s)", path, firstLine(out))
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}
