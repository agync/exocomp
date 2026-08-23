package main

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const (
	idempotencyKeyFlag = "idempotency-key"
	idempotencyKeyEnv  = "EXOCOMP_IDEMPOTENCY_KEY"
)

// addIdempotencyFlag registers the global --idempotency-key. It is declared once
// on the root, alongside --output, so every mutating subcommand inherits it.
func addIdempotencyFlag(root *cobra.Command) {
	root.PersistentFlags().String(idempotencyKeyFlag, "",
		"dedup a retried mutating command to one job (or "+idempotencyKeyEnv+"): "+
			"pass a stable per-intent value, e.g. a UUID minted once per logical "+
			"request or a hash of the command and its arguments")
}

// idempotencyKey resolves the effective key threaded onto a mutating job's
// queue.Spec: an explicit --idempotency-key wins, then EXOCOMP_IDEMPOTENCY_KEY,
// then empty. Empty is the unchanged path — the queue assigns whatever key (if
// any) it would today and never dedups. It tolerates the flag being absent so a
// subcommand executed in isolation still resolves.
func idempotencyKey(cmd *cobra.Command) string {
	if f := cmd.Flags().Lookup(idempotencyKeyFlag); f != nil && f.Changed {
		return f.Value.String()
	}
	return strings.TrimSpace(os.Getenv(idempotencyKeyEnv))
}
