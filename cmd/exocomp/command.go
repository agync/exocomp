package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func init() {
	registerCommand(newCommandCmd)
}

// newCommandCmd is the everyday ad-hoc verb: `exocomp command --host <pattern>
// -- <cmd...>` runs a bare command on a host pattern through the ansible command
// module (or the shell module with --shell). It is the ergonomic shorthand for
// `exocomp ansible -m command`; everything after `--` is the command, joined
// into the module's -a string.
func newCommandCmd() *cobra.Command {
	var (
		sourceName          string
		host                string
		limit               string
		inventory           string
		shell               bool
		become              bool
		vaultProject        string
		runSecrets          string
		runnerName          string
		requireApproval     bool
		tolerateUnreachable bool
		privileged          bool
		timeout             time.Duration
		userSession         bool
		empathyModule       string
		empathyArgs         string
		sshIdentity         []string
	)

	cmd := &cobra.Command{
		Use:   "command --host <pattern> [--shell] [--user-session] -- <cmd...>",
		Short: "Run an ad-hoc command on a host pattern (ansible command/shell module)",
		Long: "The everyday ad-hoc run. `exocomp command --host web -- systemctl status nginx` " +
			"runs the command on every host the pattern matches via the ansible command module " +
			"(no shell), from a source's checkout (--source) against its inventory or against " +
			"localhost. --shell switches to the shell module so a pipe or redirect works. " +
			"--user-session injects the active user session context ($XDG_RUNTIME_DIR, $DBUS_SESSION_BUS_ADDRESS) " +
			"for desktop, dconf, and systemctl --user commands. " +
			"Everything after `--` is the command; it is injection-guarded (a command starting " +
			"with a dash or carrying a control character is refused) and joined into the module " +
			"argument, never spread onto ansible's own flags. Same governance as `playbook run`: " +
			"sandbox, vault-id, fleet key, run-secrets, redaction, the one queue. --require-approval " +
			"gates a destructive command; --tolerate-unreachable settles an unreachable-only run " +
			"degraded rather than failed (docs/operations/adhoc-and-partial.md).",
		// The command lives after --, so no positional may precede it and at
		// least one token must follow it.
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("module") || cmd.Flags().Changed("args") {
				return errors.New("command: '-m' and '-a' are Ansible flags, not 'exocomp command' flags.\n" +
					"  - To run a command: exocomp command --host <pattern> [--shell] -- <cmd...>\n" +
					"  - To run an Ansible module: exocomp ansible --host <pattern> -m <module> [-a <args>]")
			}
			dash := cmd.ArgsLenAtDash()
			if dash > 0 {
				return fmt.Errorf("command: unexpected argument %q before --; the command goes after --", args[0])
			}
			if dash < 0 || len(args) == 0 {
				return errors.New("command: nothing after --; usage: command --host <pattern> -- <cmd...>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern, err := resolveHostPattern(host, limit)
			if err != nil {
				return err
			}
			commandArgs, err := guardCommand(args)
			if err != nil {
				return err
			}
			module := defaultAdhocModule
			if userSession {
				module = shellAdhocModule
				commandArgs = wrapUserSession(commandArgs)
			} else if shell {
				module = shellAdhocModule
			}
			identityFiles, err := parseSSHIdentityFiles(sshIdentity)
			if err != nil {
				return err
			}
			return runAdhocExit(cmd, adhocSpec{
				runbook:             "command",
				sourceName:          sourceName,
				pattern:             pattern,
				inventory:           inventory,
				module:              module,
				args:                commandArgs,
				become:              become,
				vaultProject:        vaultProject,
				runSecrets:          runSecrets,
				runnerName:          runnerName,
				requireApproval:     requireApproval,
				tolerateUnreachable: tolerateUnreachable,
				privileged:          privileged,
				timeout:             timeout,
				sshIdentityFiles:    identityFiles,
			})
		},
	}
	cmd.Flags().BoolVar(&shell, "shell", false, "run through the shell module so a pipe or redirect works")
	cmd.Flags().BoolVar(&userSession, "user-session", false, "inject active user session environment ($XDG_RUNTIME_DIR, $DBUS_SESSION_BUS_ADDRESS) on target hosts")
	cmd.Flags().StringVarP(&empathyModule, "module", "m", "", "ansible module name (hint)")
	cmd.Flags().StringVarP(&empathyArgs, "args", "a", "", "ansible module args (hint)")
	_ = cmd.Flags().MarkHidden("module")
	_ = cmd.Flags().MarkHidden("args")
	addAdhocFlags(cmd, &sourceName, &host, &limit, &inventory, &become,
		&vaultProject, &runSecrets, &runnerName, &requireApproval, &tolerateUnreachable, &privileged, &timeout, &sshIdentity)
	return cmd
}

const userSessionPrefix = `export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}" DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=/run/user/$(id -u)/bus}"; `

func wrapUserSession(cmd string) string {
	return userSessionPrefix + cmd
}

// guardCommand turns the tokens after `--` into the module's -a string, refusing
// a command that starts with a dash (it would be read as a flag) or carries a
// control character (it would split or truncate the single argv token). The
// tokens are joined with spaces, which is how ansible's command module reads its
// argument; a command needing its own quoting or a pipe uses --shell.
func guardCommand(tokens []string) (string, error) {
	if len(tokens) == 0 {
		return "", errors.New("command: nothing after --")
	}
	if strings.HasPrefix(tokens[0], "-") {
		return "", fmt.Errorf("command: %q starts with a dash; put options before --", tokens[0])
	}
	for _, tok := range tokens {
		if hasControl(tok) {
			return "", errors.New("command: a command token contains a control character")
		}
	}
	return strings.Join(tokens, " "), nil
}
