package main

import (
	"fmt"
	"os"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

const (
	assumeYesFlag = "assume-yes"
	noInputFlag   = "no-input"
)

// addConfirmFlags registers the global non-interactive contract flags. They are
// declared once on the root, alongside --output, so every subcommand inherits
// them. --assume-yes answers every destructive gate yes; --no-input asserts the
// session is non-interactive so a gate refuses rather than waiting on stdin.
func addConfirmFlags(root *cobra.Command) {
	root.PersistentFlags().BoolP(assumeYesFlag, "y", false,
		"answer yes to destructive confirmations (scripts, CI)")
	root.PersistentFlags().Bool(noInputFlag, false,
		"never prompt; refuse a confirmation instead of waiting on stdin")
}

// boolFlag reads an inherited bool flag, tolerating its absence so a subcommand
// executed in isolation (as unit tests do, without the root's persistent flags)
// still resolves to the false default.
func boolFlag(cmd *cobra.Command, name string) bool {
	if f := cmd.Flags().Lookup(name); f != nil {
		return f.Value.String() == "true"
	}
	return false
}

// stdinIsInteractive reports whether the command's stdin is a terminal a human
// could answer a prompt on. A pipe, a file, or a closed stdin is not.
func stdinIsInteractive(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// confirmDestructive gates a destructive action behind an explicit confirmation.
// preapproved is true when a command-specific confirmation flag was already
// given (e.g. --yes-really, --force); those stay valid as deprecated aliases for
// the global --assume-yes. When neither is set the action is refused with an
// actionable error naming the flag to pass — never a prompt that blocks on
// stdin. These gates have never read a yes/no from stdin, so --no-input and a
// non-TTY session behave exactly as an unconfirmed interactive one: a clear
// refusal that cannot hang. altFlag, when non-empty, names the command's own
// alias in the message.
func confirmDestructive(cmd *cobra.Command, preapproved bool, action, altFlag string) error {
	if preapproved || boolFlag(cmd, assumeYesFlag) {
		return nil
	}
	hint := "--assume-yes"
	if altFlag != "" {
		hint = altFlag + " or --assume-yes"
	}
	if boolFlag(cmd, noInputFlag) || !stdinIsInteractive(cmd) {
		return fmt.Errorf("%s needs confirmation and stdin is non-interactive: pass %s", action, hint)
	}
	return fmt.Errorf("%s needs confirmation: pass %s", action, hint)
}
