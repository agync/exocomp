package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
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
	report, fault := inspectDataDir(dataDir)
	p("data dir:     %s — %s\n", dataDir, report)
	if fault != nil {
		return fmt.Errorf("doctor: %w", fault)
	}

	p("optional tools (M0 probe subset):\n")
	for _, t := range optionalTools {
		p("  %-18s %s\n", t.name, probeTool(t))
	}

	reportSandbox(context.Background(), w)
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

// dataDirMode is the only mode the data dir may have: it holds host.key and
// the keyring, so nothing outside this uid has any business reading it.
const dataDirMode os.FileMode = 0o700

// inspectDataDir describes the data dir without touching it, and returns the
// fault that should fail the run. A doctor that created the dir would report
// on its own handiwork, and would create it under whatever mode it chose
// rather than the 0700 `exocomp init` uses.
func inspectDataDir(dir string) (string, error) {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		parent := filepath.Dir(dir)
		if err := writableDir(parent); err != nil {
			return fmt.Sprintf("absent, and %s cannot hold it (%v)", parent, err),
				fmt.Errorf("data dir cannot be created: %w", err)
		}
		return fmt.Sprintf("absent — creatable, %s is writable; `exocomp init` creates it %04o",
			parent, dataDirMode.Perm()), nil
	case err != nil:
		return fmt.Sprintf("UNREADABLE (%v)", err), fmt.Errorf("data dir not readable: %w", err)
	}

	if !info.IsDir() {
		return "NOT A DIRECTORY", errors.New("data dir is not a directory")
	}
	mode := info.Mode().Perm()
	if err := writableDir(dir); err != nil {
		return fmt.Sprintf("mode %04o — NOT WRITABLE (%v)", mode, err),
			fmt.Errorf("data dir not writable: %w", err)
	}
	if mode&^dataDirMode != 0 {
		return fmt.Sprintf("mode %04o — TOO PERMISSIVE, want %04o (chmod %04o %s)",
				mode, dataDirMode.Perm(), dataDirMode.Perm(), dir),
			fmt.Errorf("data dir mode %04o grants access beyond this user; it holds the host key",
				mode)
	}
	return fmt.Sprintf("mode %04o — writable", mode), nil
}

// writableDir reports whether this process can create entries in dir, without
// creating one. access(2) answers with the real uid, which for a process
// that is never setuid is the uid everything else here runs as.
func writableDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := unix.Access(dir, unix.W_OK|unix.X_OK); err != nil {
		return fmt.Errorf("no write access to %s: %w", dir, err)
	}
	return nil
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
