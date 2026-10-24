package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// remoteCapable lists the command paths that consult remoteClient and act on the
// selected daemon. Every other command opens the local data dir, so with a
// remote selected it must refuse: running it would silently land the change in a
// store the operator did not mean. A new remote-capable command is added here.
var remoteCapable = map[string]bool{
	"doctor":            true,
	"drain":             true,
	"drain interrupted": true,
	"restart":           true,
	"reload":            true,
	"jobs list":         true,
	"jobs get":          true,
	"jobs events":       true,
	"jobs follow":       true,
	"approval list":     true,
	"approval decide":   true,
	"source list":       true,
	"workspace list":    true,
	"workspace plan":    true,
	"workspace apply":   true,
	"workspace destroy": true,
	"playbook run":      true,
}

// remoteExempt lists the commands that run on this machine by nature (the local
// daemon, host setup, pure file or help output) and so ignore a selected remote.
// It is matched against the full path first, then the top-level name.
var remoteExempt = map[string]bool{
	"version":               true,
	"commands":              true,
	"login":                 true,
	"help":                  true,
	"completion":            true,
	"hooks validate":        true,
	"serve":                 true,
	"ssh-serve":             true,
	"setup":                 true,
	"upgrade":               true,
	sandboxCanarySubcommand: true,
}

// guardRemoteSelection is the root PersistentPreRunE. With --remote or
// EXOCOMP_REMOTE set, a command that has no remote path fails here instead of
// running against the local data dir.
func guardRemoteSelection(cmd *cobra.Command, _ []string) error {
	if _, active := selectedRemote(cmd); !active {
		return nil
	}
	path := strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()))
	top, _, _ := strings.Cut(path, " ")
	if remoteCapable[path] || remoteExempt[path] || remoteExempt[top] || strings.HasPrefix(top, "__") {
		return nil
	}
	err := fmt.Errorf("%s has no remote support: it would act on this machine's data dir, not the remote daemon; "+
		"unset --remote and EXOCOMP_REMOTE to run it here", path)
	if mode, modeErr := outputMode(cmd); modeErr == nil && mode == outputJSON {
		return failJSON(cmd, err)
	}
	return err
}
