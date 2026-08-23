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
	remoteEnv  = "EXOCOMP_REMOTE"
)

// addRemoteFlags registers --remote and --token on the root. --remote <selector>
// selects a pinned remote by its alias or by a raw https endpoint; an empty
// selector (`--remote=`) resolves to the hosts.toml default entry. A command
// with neither --remote nor EXOCOMP_REMOTE stays LOCAL (unchanged) — the
// hosts.toml default is consulted only once remote mode is entered, never to
// silently redirect a bare command off the box.
func addRemoteFlags(root *cobra.Command) {
	root.PersistentFlags().String(remoteFlag, "",
		"remote exocompd to target: a pinned name or an https endpoint (or EXOCOMP_REMOTE); "+
			"empty --remote= uses the hosts.toml default")
	root.PersistentFlags().String(tokenFlag, "",
		"bearer token for the remote daemon (or "+tokenEnv+"); overrides the stored token")
}
