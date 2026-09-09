# .contrib/tofu-e2e

A reusable OpenTofu end-to-end test rig for exocomp. It boots a throwaway
exocomp daemon, copies a tofu project into a scratch workspace, and runs the
scenario matrix from [test-plan.md](test-plan.md) — first-run state creation,
drift, plan gating + approvals, state import, run-secrets, egress, locking,
serial history, the offline provider cache, and the state-only surface.

## Requirements

- `tofu` on PATH (tested with OpenTofu v1.11.5)
- a built `bin/exocomp` (`make build` from the repo root), or pass
  `EXOCOMP_BIN=/path/to/exocomp`
- the host can reach `registry.opentofu.org` on the first run (the provider
  cache then makes later runs offline-capable)

## Run it

```sh
# full matrix
./.contrib/tofu-e2e/run-e2e.sh

# one scenario
./.contrib/tofu-e2e/run-e2e.sh --scenario S03

# keep the daemon and its data for inspection after a failure
./.contrib/tofu-e2e/run-e2e.sh --keep-daemon --keep-data
```

Results append to `.agents/brain/tofu-e2e/results/<date>.log`; web screenshots
land under `.agents/brain/screenshots/tofu-e2e/<scenario>/`.

## Layout

```
lib/daemon.sh     start/stop the throwaway daemon + mint operator/readonly tokens
lib/faces.sh      per-surface drivers: CLI (local), web (screenshot hook), MCP (JSON-RPC)
lib/assert.sh     assertion helpers
projects/a-local  tofu root module: local/null provider, no external target
projects/a-adopt  same, without a backend block (the S04 adoption case)
run-e2e.sh        the scenario driver
test-plan.md      the scenario matrix and what each one proves
```

## Scenarios

See [test-plan.md](test-plan.md). Each scenario is self-contained and may be
run alone; a failure is reported as a finding (the matrix continues).

## Known gaps

Tracked in `.agents/brain/tofu-e2e/todo.md`:

- **G2** — the executor's `OfflineProviders` option is not reachable from any
  CLI/API surface, so the full offline-provider-mirror enforcement cannot be
  driven end-to-end yet. S09 asserts the warm-cache half only.
- G1 (state adoption vs TF_ENCRYPTION) is **fixed** — `exocomp workspace adopt`
  migrates pre-existing local state; S04 asserts the full flow.
