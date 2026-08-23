package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runRootSilenceAdd drives `silence add` through a real root command so the
// global --assume-yes/--no-input persistent flags exist, exactly as a shell
// invocation has them. stdin is a bytes.Buffer, i.e. non-interactive.
func runRootSilenceAdd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(&bytes.Buffer{})
	root.SetArgs(append([]string{"silence", "add"}, args...))
	err := root.Execute()
	return out.String(), err
}

// TestConfirmContractGlobalSilence exercises the non-interactive contract on a
// confirm-gated command: --assume-yes proceeds, --no-input without it refuses
// clearly (and never hangs, because stdin is never read), and the deprecated
// --yes-really alias still confirms.
func TestConfirmContractGlobalSilence(t *testing.T) {
	initForSource(t)

	// No confirmation on a non-interactive session: a clear refusal naming the
	// flags, not a hang.
	out, err := runRootSilenceAdd(t, "--global", "--for", "1h")
	if err == nil {
		t.Fatalf("global silence without confirmation succeeded: %s", out)
	}
	if !strings.Contains(out, "--assume-yes") || !strings.Contains(out, "--yes-really") {
		t.Errorf("refusal did not name the flags to pass: %q", out)
	}

	// --no-input is explicit about the same refusal.
	out, err = runRootSilenceAdd(t, "--global", "--for", "1h", "--no-input")
	if err == nil {
		t.Fatalf("global silence with --no-input succeeded: %s", out)
	}
	if !strings.Contains(out, "non-interactive") {
		t.Errorf("--no-input refusal did not explain itself: %q", out)
	}

	// --assume-yes proceeds without a prompt.
	if out, err := runRootSilenceAdd(t, "--global", "--for", "1h", "--assume-yes"); err != nil {
		t.Fatalf("--assume-yes did not proceed: %v (%s)", err, out)
	}

	// The deprecated alias still confirms.
	if out, err := runRootSilenceAdd(t, "--global", "--for", "1h", "--yes-really"); err != nil {
		t.Fatalf("--yes-really alias broke: %v (%s)", err, out)
	}
}

// TestConfirmDestructiveDecisionTable drives the helper through a real command
// tree — global flags on the root, a --force alias on the leaf — so the flag
// merge is the same one production sees. It pins the whole decision table,
// including that a bare non-interactive session refuses (these gates never
// prompted, so default behavior is unchanged).
func TestConfirmDestructiveDecisionTable(t *testing.T) {
	run := func(args ...string) error {
		root := &cobra.Command{Use: "root", SilenceUsage: true, SilenceErrors: true}
		addConfirmFlags(root)
		var preapproved bool
		sub := &cobra.Command{Use: "sub", RunE: func(cmd *cobra.Command, _ []string) error {
			return confirmDestructive(cmd, preapproved, "act", "--force")
		}}
		sub.Flags().BoolVar(&preapproved, "force", false, "")
		root.AddCommand(sub)
		root.SetIn(&bytes.Buffer{}) // non-interactive stdin
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"sub"}, args...))
		return root.Execute()
	}

	// Neither flag: refuse with an actionable message naming both flags.
	err := run()
	if err == nil {
		t.Fatal("unconfirmed action should refuse")
	}
	if !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "--assume-yes") {
		t.Errorf("refusal message not actionable: %v", err)
	}

	// --no-input names the non-interactive reason and still refuses.
	if err := run("--no-input"); err == nil || !strings.Contains(err.Error(), "non-interactive") {
		t.Errorf("--no-input should refuse and explain: %v", err)
	}

	// The legacy alias proceeds.
	if err := run("--force"); err != nil {
		t.Errorf("--force alias should proceed: %v", err)
	}

	// The global --assume-yes proceeds.
	if err := run("--assume-yes"); err != nil {
		t.Errorf("--assume-yes should proceed: %v", err)
	}
}
