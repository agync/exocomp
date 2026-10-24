package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/wire"
)

// Every runnable command is in exactly one of the four sets below
// (Decision 0026), and TestEveryCommandIsClassified fails on one that is not.
// Each set is keyed by the full command path, never a parent's name, so a
// subcommand added later is classified on its own.
// With a remote selected, the guard lets a remote-capable or exempt command run,
// and refuses the rest before they open this machine's data dir.

// remoteCapable lists the command paths that consult remoteClient and act on the
// selected daemon.
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
	"ansible":           true,
	"command":           true,
}

// remoteExempt lists the commands that run on this machine by nature (a daemon
// face, host setup, this binary's upgrade, the restore path, the client's own
// login, pure file or help output) and so ignore a selected remote.
var remoteExempt = map[string]bool{
	"version":               true,
	"commands":              true,
	"login":                 true,
	"help":                  true,
	"completion bash":       true,
	"completion fish":       true,
	"completion powershell": true,
	"completion zsh":        true,
	"hooks validate":        true,
	"serve":                 true,
	"scheduler run":         true,
	"web":                   true,
	"ssh-serve":             true,
	"setup":                 true,
	"upgrade":               true,
	sandboxCanarySubcommand: true,
	// The restore path runs where it is invoked, whatever hosts.toml names:
	// packaging/recovery.sh calls restore, and a lost daemon is the case it
	// exists for.
	"restore":  true,
	"rollback": true,
}

// localOnly lists the commands that stay on the daemon's own host on purpose,
// each with the reason a selected remote refuses it: they need the host's keys,
// act on what a restart of the host left, or would carry a credential over the
// API. They run there with --local.
var localOnly = map[string]string{
	"init":              "it creates the host identity and keyring",
	"identity migrate":  "it re-wraps the keyring with the host key",
	"seal enable":       sealReason,
	"seal disable":      sealReason,
	"seal status":       sealReason,
	"kit derive":        "it derives from this machine's keyring, and --list reads its store",
	"kit verify":        "it renders the kit from this machine's keyring",
	"recovery-kit":      "it renders the kit from this machine's keyring",
	"backup drill":      "it restores a snapshot with the host's keys",
	"token list":        "it reads the host's token store, and no API route lists tokens",
	"token new":         tokenMintReason,
	"token revoke":      "API tokens are revoked on the host, never over the API",
	"token rotate":      tokenMintReason,
	"workspace token":   "it prints a state-backend bearer",
	"workspace backend": "with --config it prints the state-backend bearer",
	"workspace adopt":   "it migrates a tofu dir's local state and serves the state backend itself",
	"litestream config": litestreamReason,
	"litestream status": litestreamReason,
	"litestream target": litestreamReason,
	"tui":               "it reads the data dir; reach a daemon's TUI with ssh-serve",
	"approval link":     "a decision link is a capability that never crosses the API",
	"jobs redrive":      "it re-drives the jobs a restart of the host cut off, from the host's own store",
}

const (
	sealReason       = "the transit seal wraps the host's data key"
	tokenMintReason  = "API tokens are minted on the host, never over the API"
	litestreamReason = "it configures the replication sidecar beside the host's database"
)

// remotePending lists the commands that will act on a selected remote once
// their API route exists. Until then a selected remote refuses them; --local
// runs them against this machine's data dir.
var remotePending = map[string]bool{
	"backup add-target":  true,
	"backup list":        true,
	"backup run":         true,
	"bootstrap run":      true,
	"bootstrap status":   true,
	"check add":          true,
	"check apply":        true,
	"check disable":      true,
	"check enable":       true,
	"check export":       true,
	"check list":         true,
	"check probe":        true,
	"check rm":           true,
	"check snooze":       true,
	"check test":         true,
	"check unsnooze":     true,
	"check update":       true,
	"cli-tool add":       true,
	"cli-tool list":      true,
	"cli-tool rm":        true,
	"event explain":      true,
	"event list":         true,
	"handoff run":        true,
	"handoff status":     true,
	"hooks show":         true,
	"host known-hosts":   true,
	"host onboard":       true,
	"inventory add":      true,
	"inventory list":     true,
	"inventory rm":       true,
	"mute add":           true,
	"mute list":          true,
	"mute rm":            true,
	"netgroup add":       true,
	"netgroup apply":     true,
	"netgroup export":    true,
	"netgroup list":      true,
	"netgroup rm":        true,
	"netgroup show":      true,
	"netgroup update":    true,
	"netgroup usage":     true,
	"playbook vendor":    true,
	"policy add":         true,
	"policy list":        true,
	"policy rm":          true,
	"provision run":      true,
	"provision status":   true,
	"rule add":           true,
	"rule disable":       true,
	"rule enable":        true,
	"rule list":          true,
	"rule rm":            true,
	"run":                true,
	"run status":         true,
	"run-cli":            true,
	"run-secret add":     true,
	"run-secret list":    true,
	"run-secret rm":      true,
	"runner add":         true,
	"runner import":      true,
	"runner list":        true,
	"runner rm":          true,
	"runner show":        true,
	"schedule add":       true,
	"schedule disable":   true,
	"schedule enable":    true,
	"schedule list":      true,
	"schedule rm":        true,
	"secrets add":        true,
	"secrets list":       true,
	"secrets replace":    true,
	"secrets rm":         true,
	"silence add":        true,
	"silence list":       true,
	"silence rm":         true,
	"source add":         true,
	"source egress":      true,
	"source rm":          true,
	"source run-secrets": true,
	"source sync":        true,
	"workspace add":      true,
	"workspace egress":   true,
	"workspace offline":  true,
	"workspace prune":    true,
	"workspace unlock":   true,
}

// commandKey is a command's path below the root, and its top-level name.
func commandKey(cmd *cobra.Command) (path, top string) {
	path = strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()))
	top, _, _ = strings.Cut(path, " ")
	return path, top
}

// remoteRefusal is the guard's refusal. It carries wire.ErrRemoteUnsupported so
// the --output json envelope names the code remote_unsupported, not internal.
type remoteRefusal string

func (e remoteRefusal) Error() string { return string(e) }
func (e remoteRefusal) Unwrap() error { return wire.ErrRemoteUnsupported }

// guardRemoteSelection is the root PersistentPreRunE. With a remote selected
// (--remote, EXOCOMP_REMOTE, or the hosts.toml default), a command that has no
// remote path fails here instead of running against this machine's data dir.
func guardRemoteSelection(cmd *cobra.Command, _ []string) error {
	path, top := commandKey(cmd)
	// cobra's hidden __complete helpers serve shell completion and touch no store.
	if remoteExempt[path] || strings.HasPrefix(top, "__") {
		return nil
	}
	sel, err := selectRemote(cmd)
	if err == nil && (!sel.active || remoteCapable[path]) {
		return nil
	}
	if err == nil {
		if reason, ok := localOnly[path]; ok {
			err = remoteRefusal(fmt.Sprintf("%s runs only on the daemon's host: %s; run it there with --local",
				path, reason))
		} else {
			err = remoteRefusal(fmt.Sprintf("%s has no remote path yet: with a remote selected it would act "+
				"on this machine's data dir, not the daemon's; pass --local to run it here", path))
		}
	}
	if mode, modeErr := outputMode(cmd); modeErr == nil && mode == outputJSON {
		return failJSON(cmd, err)
	}
	return err
}
