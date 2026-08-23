package main

import (
	"errors"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/wire"
)

// exitCodeFor maps a command's returned error onto the process exit code, via
// the shared wire taxonomy (AGENTS.md invariant 3: subprocess correctness is
// exit-code-only). It is the ONE site where the taxonomy meets the real
// process boundary: a local error carrying a store/reactive sentinel resolves
// through wire.ExitFor; a remote client.APIError resolves through its own
// ExitCode(), which consults the identical wire.ExitForCode table — so
// `jobs get <id>` exits 3 for a not-found id whether that id was decided by
// the local store or a remote daemon. An error that matches no taxonomy entry
// (including nil, which callers never pass here) exits 1, unchanged from
// before this mapping existed.
//
// It does NOT touch the deliberate per-command os.Exit(code) calls already in
// workspace.go/playbook.go/adhoc.go/vendor.go (a run's own outcome, tofu
// plan's changes-present 2): those call os.Exit directly inside RunE and never
// reach here, by construction — main's Execute() never returns to observe them.
func exitCodeFor(err error) int {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ExitCode()
	}
	return wire.ExitFor(err)
}
