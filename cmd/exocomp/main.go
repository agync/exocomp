// Command exocomp is the exocompd control-plane CLI.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/version"
)

// commandFactories collects per-file subcommand constructors so each file can
// register its own command without editing a shared list.
var commandFactories []func() *cobra.Command

func registerCommand(f func() *cobra.Command) {
	commandFactories = append(commandFactories, f)
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(exitCodeFor(err))
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "exocomp",
		Short:         "exocomp homelab control plane",
		SilenceUsage:  true,
		SilenceErrors: false,

		PersistentPreRunE: guardRemoteSelection,
	}

	addOutputFlag(root)
	addIdempotencyFlag(root)
	addConfirmFlags(root)
	addRemoteFlags(root)

	root.AddCommand(newVersionCmd())
	root.AddCommand(newDoctorCmd())
	for _, f := range commandFactories {
		root.AddCommand(f())
	}

	assignCommandGroups(root)
	fillExamples(root)

	return root
}

// assignCommandGroups organizes the top-level verbs into titled sections in the
// help output. It changes nothing about how a command is invoked or what the
// `commands` machine tree emits — grouping is presentation only, so the
// agent-facing surface is unchanged. A command not in the map falls under
// cobra's "Additional Commands".
func assignCommandGroups(root *cobra.Command) {
	groups := []*cobra.Group{
		{ID: "run", Title: "Run & schedule:"},
		{ID: "source", Title: "Sources & inventory:"},
		{ID: "secret", Title: "Secrets & access:"},
		{ID: "reactive", Title: "Automation & policy:"},
		{ID: "fleet", Title: "Hosts & provisioning:"},
		{ID: "backup", Title: "Backup & recovery:"},
		{ID: "ops", Title: "Service & maintenance:"},
	}
	for _, g := range groups {
		root.AddGroup(g)
	}
	byName := map[string]string{
		"run": "run", "run-cli": "run", "playbook": "run", "ansible": "run",
		"command": "run", "workspace": "run", "schedule": "run", "jobs": "run",
		"source": "source", "inventory": "source", "hooks": "source", "cli-tool": "source",
		"secrets": "secret", "token": "secret", "run-secret": "secret", "kit": "secret",
		"identity": "secret", "seal": "secret", "recovery-kit": "secret", "login": "secret",
		"policy": "reactive", "rule": "reactive", "silence": "reactive", "mute": "reactive",
		"event": "reactive", "approval": "reactive",
		"host": "fleet", "ssh-serve": "fleet", "bootstrap": "fleet", "provision": "fleet", "handoff": "fleet",
		"backup": "backup", "restore": "backup", "rollback": "backup", "litestream": "backup",
		"serve": "ops", "doctor": "ops", "setup": "ops", "upgrade": "ops", "drain": "ops",
		"init": "ops", "web": "ops", "tui": "ops", "runner": "ops",
	}
	for _, c := range root.Commands() {
		if gid, ok := byName[c.Name()]; ok {
			c.GroupID = gid
		}
	}
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
