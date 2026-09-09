# tofu-e2e test plan

The scenario matrix. Each row names the scenario, what it proves about exocomp,
and how it is driven. A scenario is a *finding* when it fails: the rig exists to
surface gaps, not to confirm a rosy picture.

| # | Scenario | Proves | Drive | Status |
|---|---|---|---|---|
| S01 | First-run state creation | GET 404 → empty → apply POSTs serial 1; state encrypted at rest (TF_ENCRYPTION pbkdf2); plan exits 2 = changes, job still succeeds | CLI: `workspace add` → `plan` (exit 2) → `apply` → marker written | PASS |
| S02 | Plan → drift | out-of-band change re-plans to exit 2; drift is a fact, not a failed run | CLI: mutate marker, `plan` exits 2 | PASS |
| S03 | Plan gating + approvals | saved plan artifact sealed; require-approval policy pends the apply; `approval decide --approve` releases it and the job runs | CLI: `policy add … --action require_approval`, `plan`, `apply --plan`, `approval decide --approve`, poll job | PASS |
| S04 | State import / adoption | `tofu init -migrate-state` onto the HTTP backend takes over existing local state | CLI + manual tofu with local backend | **FAIL = G1** |
| S05 | Run-secrets as TF_VAR_ | sealed secret bound to `TF_VAR_*` reaches the provider's env; value never in logs/events | CLI: `secrets add`, `run-secret add`, `apply --run-secrets`; marker takes the secret value | PASS |
| S06 | Egress declaration | `workspace egress` adds/clears extra outbound ports for tofu runs | CLI: `workspace egress <ws> 6443` then clear | PASS |
| S07 | Locking + stale-lock unlock | audited `workspace unlock` breaks a held lock | CLI: `workspace unlock` | PASS |
| S08 | Serial history + prune | N applies produce N serials; `prune --keep --days` trims past the budget | CLI: 3× (mutate, plan, apply), `prune` | PASS |
| S09 | Offline provider cache | first init populates the plugin cache; warm-cache runs reuse it | CLI + cache dir assertion | PASS (enforcement = G2) |
| S10 | State-only (mode-A) | `workspace add --state-only` emits the backend block + bearer for an external tofu | CLI: state-only add emits the block | PASS |

## Driving surfaces

- **CLI**: local mode against the daemon's data dir (`XDG_DATA_HOME`),
  `--output json` where a machine-readable assertion helps.
- **Web**: the token-gated listener on `0.0.0.0:8642`; screenshots via the
  playwright-cli skill into `.agents/brain/screenshots/tofu-e2e/`.
- **MCP**: JSON-RPC `POST /mcp` with the operator token; tools
  `list_workspaces`, `workspace_plan`, `workspace_apply`, `get_drift`,
  `approve_job`/`reject_job`.

A scenario's three legs must agree on the wire bytes (CLI `--output json`, the
web render, and the MCP resource/tool result come from the same
`internal/wire` envelope — the documented property the rig re-proves).

## Notes on repeatability

- Each invocation copies the tofu project fresh into the scratch workspace, so
  the working dir is reset while the SQLite state persists. A standalone rerun
  of S02/S08/S09 therefore legitimately sees drift (marker absent vs recorded
  state) — the scenarios assert the *behavior*, not a specific plan exit,
  except where the exit is the point.
- The daemon data dir lives under `/tmp/exocomp-throwaway/tofu-e2e` by default
  (`EXOCOMP_E2E_DATA` to override). `--keep-data` reuses it; without it, a run
  starts clean.
