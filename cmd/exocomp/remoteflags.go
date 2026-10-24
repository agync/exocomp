package main

import "github.com/spf13/cobra"

// The client-mode inputs, declared once on the root so every subcommand inherits
// them (alongside --output and the confirm flags). --remote names the remote
// daemon endpoint and is deliberately NOT --host: --host is already the ansible
// host PATTERN on the command/adhoc verbs, a different axis (which managed nodes
// a play targets, not which daemon the CLI talks to). Reusing it would conflate
// the two. This is the one deviation from agent-interface.md §3.3, which proposed
// --host for the daemon; it is renamed here to keep the ansible pattern intact.
const (
	remoteFlag = "remote"
	localFlag  = "local"
	remoteEnv  = "EXOCOMP_REMOTE"
)

// addRemoteFlags registers --remote, --local and --token on the root.
// --remote <selector> selects a pinned remote by its alias or by a raw https
// endpoint; an empty selector (`--remote=`) resolves to the hosts.toml default.
// With no --remote and no EXOCOMP_REMOTE, the hosts.toml default still selects
// its remote, so a machine whose daemon lives elsewhere sends every command
// there. --local is the one way to run against this machine's data dir while a
// remote is configured.
func addRemoteFlags(root *cobra.Command) {
	root.PersistentFlags().String(remoteFlag, "",
		"remote exocompd to target: a pinned name or an https endpoint (or EXOCOMP_REMOTE); "+
			"empty --remote= uses the hosts.toml default, which also applies when neither is set")
	root.PersistentFlags().Bool(localFlag, false,
		"run against this machine's data dir even when a remote is configured")
	root.PersistentFlags().String(tokenFlag, "",
		"bearer token for the remote daemon (or "+tokenEnv+"); overrides the stored token")
}
