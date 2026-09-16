# Plan — checks / probes / netgroups (post-v1 program)

Companion to `2026-09-15-research-checks-netgroups.md` (same directory). That
document found the gaps; this one sequences the work, and — because the brief is
"ensure these features are useful" — makes every phase prove a *named operator
scenario* end-to-end before it can be called done.

Status: proposal for review, not committed work. Once the operator picks a
scope, this becomes `.agents/workflows/checks-plan.md` and lanes are dispatched
from it (lane-brief-authoring), in the `mNN` style but as a post-v1 program.

## Progress

- **C0 core — landed** (migrations 0037/0038/0039; sweep stores netgroup +
  duration, probes in parallel, gated by `--check-canary`; snooze suppresses
  emission while state advances). Plan review folded first (B1–B3).
- **C0 surfaces — landed** (`/metrics` check gauges + `CheckTargetDownAtHome`/
  `CheckSnoozed` alerts; `check snooze/unsnooze`; NETGROUP/DURATION columns;
  web stored-location + duration + down-at-home; web check edit). A follow-up
  fix made `UpdateCheck` prune dropped targets' state rows.
- **C2 — landed** (migration 0040 dual-table rebuild widening the kind CHECK;
  `internal/probe` shared runner; checks gain dns/tls/metric; verify folded
  onto the runner; non-IP targets classify to no netgroup via `probe.Location`).
- **C1 + C2 surfaces — landed** (CLI kind flags; `check probe`/`check test`,
  provably side-effect free; web kind form + edit).
- **C3 — landed** (netgroup exclusions, `update`/`show`/`usage`, reserved
  `away`, unified `labels["netgroup"]` for webhooks and checks).
- **C4 — landed** (MCP read/probe/write tools + `/api/v1` write routes).
- **C5 — landed, opt-in** (migration 0041 aggregate config; aggregate event with
  `Hosts`; governed `--limit-hosts` expansion validated against the named
  check's target set; reserved `*` state row filtered).
- **C6 — landed** (`check/netgroup export` + `apply -f`, idempotent, `--prune`
  confirmed; apply preflights every row before writing).

**Not yet exercised (needs the operator's homelab):** S2's real expiry ladder on
the operator's own cert, and S9's "one run over the present subset" dogfood.
The C5 feature is default-off until S9 is shown useful; C2's cert kind is
shippable but unproven against a real cert.

Gate note: each lane ran `make check-race` for its touched packages at
integration; the combined `main` carries the full `make check-race` result.


---

## 1. Goal

Make the check/probe/netgroup layer carry its weight as exocomp's own
monitoring substrate: one probe vocabulary, usable from events, gates, and
agents; a netgroup that is a configuration primitive and not only a label; and
honest, actionable status. Success is not "more kinds shipped" — it is the
scenarios in §3 actually running on the operator's own homelab.

## 2. The usefulness contract (applies to every phase)

A phase is **not done** because tests are green. It is done when:

1. **A named scenario (§3) runs end-to-end** on a real host and the transcript
   is pasted into the lane report — lima for the isolation cases, the
   operator's homelab for the presence cases. No scenario → no phase.
2. **A kill criterion was tested.** Each scenario names what "not useful" looks
   like (e.g. "still standing up a second monitor", "false-positives on the
   uplink"). If the dogfood window (default 7 days) shows the kill condition,
   the surface is removed rather than carried.
3. **Guard tests are mutation-checked** (revert the fix → red), per
   `PROCESS_REVIEW.md`.
4. **Agent-facing surfaces pass `mcp-comprehension-audit`**, and new write
   tools pass `security-red-blue`, before they merge.
5. **Docs move with the code** (invariant 8): `docs/operations/reactive.md`,
   the design doc, `docs/operations/CHANGELOG.md`, and an ADR for any
   hard-to-reverse interface (probe type, aggregate-event semantics).

## 3. Named scenarios (the definition of useful)

| id | scenario | proves |
|---|---|---|
| S1 | "Update my laptop when it comes home" keeps working unchanged | no regression through the probe unification |
| S2 | "Warn me 21 days before the `*.lan` cert expires" | TLS check, no external cert monitor |
| S3 | "Tell me the NAS is up but answering slowly" | latency recorded + threshold |
| S4 | "Don't page me when my own uplink drops" | network-outage canary |
| S5 | "Show which down targets should be home" | netgroup persisted on state |
| S6 | "Let the agent list/probe a check" | MCP + write API parity |
| S7 | "Watch checks in Grafana, not a second dashboard" | `/metrics` gauges |
| S8 | "Home, except the guest VLAN" | netgroup exclusions |
| S9 | "One run against every host currently present" | aggregate event + `--limit-hosts` |

Each phase below names the scenarios it must demonstrate.

---

## 4. Binding decisions (resolve before lanes branch)

- **D1 — one probe package.** New `internal/probe`: a tagged-union probe with
  per-kind validate/run, consumed by `reactive` (events) and `verify` (gates).
  `verify` keeps its argv encoding and job routing; its kind implementations
  move. This is the load-bearing decision; get an ADR.
- **D2 — shared probe runner (revised twice: plan review + FK reality).** The
  one probe vocabulary is realized as a new `internal/probe` package (a union
  `Spec` + `Run`), consumed by both `reactive` checks and `verify` gates. Store
  targets stay flat and gain additive optional fields (`DNSName`/`ExpectAddr`/
  `MinDays`/`Op`/`Value`); `targets_json` is NOT restructured, so no golden-test
  rewrite. **One kind per check** stays: `check_row.kind` is retained and must
  equal the target kind. Widening its `CHECK` needs a rebuild, and because
  `check_state` has an incoming FK `REFERENCES check_row(id) ON DELETE CASCADE`,
  the migration rebuilds BOTH tables (child dropped before parent, parent's new
  name renamed back; see `migration-authoring/references/rebuild-table.md`) and
  carries a row-survival upgrade test. Non-IP kinds (`dns`/`tls`/`metric`)
  classify to no netgroup through a `probe.Location() (netip.Addr, bool)`
  accessor instead of assuming `t.Address` is an IP literal — the four unguarded
  sites are `internal/wire/schema.go:849`, `internal/web/checks.go:264`,
  `internal/web/api.go:656`, `cmd/exocomp/check.go:349`.

- **D3 — probe execution stays in the daemon.** `check probe` writes no state
  and emits no event; it is a read-scope operation on every face.
- **D4 — netgroup persistence.** `check_state` gains the classified group;
  classification at sweep time is authoritative.
- **D5 — aggregate semantics** (C5 only): a check-level event keyed
  `check:<check>:aggregate`; `Event.Hosts` populated only by sweep sources,
  per the trusted-source line in the design (§3.3, §7.4).
- **D6 — migration reservation.** Numbers 0037+ are reserved to this program;
  each lane claims one at brief time, never renumbers. Every introduction
  migration carries a pinned in-place-upgrade test (`migration-authoring`).

### Plan-review findings folded (2026-09-15)

An adversarial read-only review verified every claim against code. Blockers it
found, all folded above:

- **B1** — `check_row.kind`'s inline CHECK must be rebuilt, not JSON-transformed (D2, C2).
- **B2** — four netgroup paths call `netip.ParseAddr`/`MustParseAddr` on what
  would become non-IP targets; fixed by `probe.Location()` (D2, C2).
- **B3** — per-row vs per-target kind was undefined; D2 now fixes one kind per check.
- **S1–S5** — the real shape contract is `wire.CheckTargetView`/`NewCheckList`
  (not just `CheckView`); generated templ, `scanCheckState`/`checkStateColumns`/
  `SetCheckState`, migration upgrade tests, and byte-level golden tests all move
  in the same lanes. C0.1 and C2 now name them.


---

## 5. Phases

### C0 — status-quo fixes (small, non-breaking, no new architecture)

Ship first: these make the current feature honest and are independent of D1–D5.

- **C0.1 §4.1 down + home.** Persist the netgroup on `check_state`: the column
  plus `checkStateColumns`/`scanCheckState`/`SetCheckState`, the cold-start seed
  and update in `internal/reactive/checks.go:159`, the three read paths that
  currently recompute (`internal/web/checks.go:249`, `internal/web/api.go:632`,
  `cmd/exocomp/check.go:342`), and `wire.CheckTargetView`/`NewCheckList`
  (`internal/wire/schema.go:805,831`). Unblocks the attention card, `check list
  --netgroup`, and the S7 gauge label.
- **C0.2 §4.4 check metrics.** Add `exocomp_check_target_up{check,target,netgroup}`,
  `exocomp_check_last_change_timestamp_seconds`,
  `exocomp_check_probe_duration_seconds` to `internal/metrics/metrics.go`, beside
  the drift gauges. Add the matching alert rule to `alerts.yml`.
- **C0.3 §4.6 bounded parallel probing.** Worker pool in `sweepCheck`; keep
  per-target state application single-threaded.
- **C0.4 §4.2 network-outage canary.** Probe a canary (gateway or a local
  service) before a sweep declares mass-down; mark the sweep degraded and skip
  emission. Record the verdict so `check list` distinguishes target-down from
  vantage-down.
- **C0.5 §4.5 check snooze.** Check-level mute with mandatory expiry, reusing
  the silence model; suppresses emission, does not pause probing.
- **C0.6 §3.3 web check edit.** Add the update dialog calling `UpdateCheck`
  (state-preserving by target name), mirroring `check add`.

**Scenarios:** S3 (partial), S4, S5, S7.
**Usefulness check:** S5 is validated by the operator glancing at `/checks`
after a known target is deliberately unplugged at home; S4 by pulling the WAN
uplink and confirming no page.
**Migrations:** 0037 (check_state netgroup), 0038 (check snooze).
**Gate:** real transcript per fix; mutation check on the canary and the netgroup
persistence; `make check-race`.

### C1 — on-demand probe and diagnostics (small, non-breaking)

- `internal/probe` extracted *just enough* to expose a single-target run
  (C2 finishes the union).
- `exocomp check probe <name> [--target t]` — prints per-target result,
  duration, and classification; writes nothing.
- `exocomp check test --kind tcp --target ...` pre-add dry run.
- **Scenarios:** S3, S6 (first half).
- **Usefulness check:** the operator must be able to answer "is this check
  pointed at the right thing" without waiting an interval; kill criterion is
  "still used `check list` after add to guess".
- **Gate:** CLI output pasted; the probe-writes-no-state property asserted by a
  test that snapshots the store before/after.

### C2 — one probe vocabulary (medium, breaking, D1/D2)

- `internal/probe`: union `Spec` + `Run` + per-kind `Validate`, kinds `tcp`,
  `ping`, `http`, `dns`, `tls`, `metric`. Move `verify`'s DNS/TLS/metric
  implementations in and ADD the `tcp`/`ping` kinds `verify` lacks; keep
  `verify.Encode/Decode` and `JobKind` unchanged for gate rows. The http dial
  guard is an option (`AllowLoopback`): checks refuse loopback, gates allow it.
- **Rebuild `check_row` + `check_state`** together to widen the kind CHECK
  (`0030_check.sql:12`); `check_state`'s incoming FK is why the child is dropped
  before the parent. Migration carries a pinned upgrade test that seeds rows at
  the prior version and proves they survive.
- **`probe.Location() (netip.Addr, bool)`** becomes the single accessor the four
  netgroup read paths use (D2); `dns`/`tls`/`metric` classify to no netgroup
  (like `http` today), never `MustParseAddr`.
- `check add` gains `--kind dns`, `tls` (with `--min-days`), `metric`
  (`--url --op --value`); generated `view_checks_templ.go` and the kind
  `<select>` widened (`internal/web/checks.templ:60`).
- Unify the http SSRF guard so checks and gates share one dial control.
- **Ordering note:** this runs before C1 (build the runner, then expose it) to
  avoid extracting a probe path twice.
- **Golden/shape tests** that pin the old JSON (`internal/web/checks_test.go:1070`
  and the `store.CheckTarget` fixtures across store/reactive/web/cmd) are
  rewritten in the same lane — the review flagged these as an omission.
- **Scenarios:** S2 (cert-expiry), S8 not yet.
- **Usefulness check:** S2 runs on the operator's own web cert and the 21-day
  ladder pings the phone before the real expiry. If the operator still reaches
  for a separate cert monitor, remove the kind.
- **Gate:** a real TLS check against the operator's cert surfaces days-to-expiry;
  a metric check against a real endpoint crosses its threshold and fires; S1
  regression test proves the laptop scenario is byte-identical.
- **Review:** plan review before branching (D1 is expensive); ADR for the probe
  type.

### C3 — netgroup as a primitive (small–medium)

- `netgroup update <name> <cidr[,...]>` (replace a group's set), `netgroup show
  <addr>` (classify one address, print the group and the winning prefix, or
  `away`), and exclusions (`home 192.168.0.0/16,!192.168.5.0/24`).
- Refuse a group named `away` at write time (it is byte-identical to the miss
  label today — a documented wart).
- `netgroup usage [<name>]` — which check targets and which observed webhook
  peers currently land in a group; makes the classification inspectable rather
  than inferred.
- First-class `event.netgroup` for every source: ingress stamps
  `labels["netgroup"]` from the peer (keeping `peer_netgroup` as the explicit
  network-path name), so a webhook rule and a check rule read the same key.
- **Dropped from the original plan:** `check add --target-group` expansion. A
  netgroup is a CIDR set, not an enumerable host list, so "expand the group into
  targets" is not well-defined (a /16 is not a target set). Revisit only if
  netgroups ever carry explicit host entries.
- **Scenarios:** S8.
- **Usefulness check:** S8 expressed in one netgroup; the operator should not
  need a rule per VLAN. Kill criterion: still writing exclusion logic in CEL.
- **Gate:** exclusion test (a guest-VLAN address reads `away` while the rest of
  the same /16 reads the group); `netgroup show` classification matches the
  sweep's stored value; `netgroup update` replaces the set without disturbing
  check_state; a webhook event carries `labels["netgroup"]`.
- **Migration:** none — the `prefixes` TEXT column keeps its encoding, with a
  leading `!` marking an exclusion.

### C4 — agent and API parity (medium, non-breaking)

- MCP read tools `list_checks`, `get_check`, `list_netgroups`; write tools
  `check_add`, `check_update`, `check_enable`, `check_disable`, `check_rm`,
  `probe_check`, `netgroup_add`, `netgroup_update`, `netgroup_rm` under the
  write capability floor.
- `/api/v1` write routes for the same, over existing store/service verbs.
- `probe_check` stays read-scope (D3).
- **Scenarios:** S6.
- **Usefulness check:** run the scenario as an actual agent task ("the laptop
  check is noisy, disable it and confirm the next event is suppressed"); if the
  agent needs a human to run the CLI, the surface failed.
- **Gate:** `mcp-comprehension-audit` on the new tools; `security-red-blue` on
  the write tools (scope floors, probe abuse); real agent transcript.
- **Note:** `verify` already has no MCP surface; keep the two probe surfaces
  consistent or explicitly document why.

### C5 — aggregate checks + set-event (large, breaking, D5)

- Check-level aggregate (`--min-up N` / all), one `check:<check>:aggregate`
  event, and the deferred `Event.Hosts` + `--limit-hosts` expansion under the
  trusted-source line.
- New dedup/cooldown keying for the aggregate (the per-source split in the
  design §5.5 does not cover it).
- **Scenarios:** S9.
- **Usefulness check:** the "one run over the present subset" need must be
  demonstrated on real hosts before the lane branches — if per-target rules
  already cover the operator's cases, **kill C5** (the research flagged it as
  unproven).
- **Gate:** a multi-target check raising exactly one aggregate event; a
  `--limit-hosts` run covering exactly the present set; a forged resolve cannot
  widen it.

### C6 — config as code (medium, non-breaking)

- `exocomp check apply -f checks.yaml` / `netgroup apply -f`, plus `export`,
  idempotent by name, reusing the C2 probe schema.
- **Scenarios:** operator restores a fresh box's monitor set from one file.
- **Usefulness check:** the operator's real probe estate must round-trip through
  the file and converge; kill criterion: config drifts from git within a week.
- **Gate:** apply→export→apply is a no-op; a live estate converges.

---

## 6. Lane / ownership sketch (finalized at dispatch)

| Phase | Lanes | Owns exclusively |
|---|---|---|
| C0 | store, metrics, sweep, web | `internal/store/check.go`, `internal/metrics/**`, `internal/reactive/checks.go`, `internal/web/checks*.go`, `internal/web/view_checks.templ` |
| C1 | probe-cli | `internal/probe/**` (new), `cmd/exocomp/check.go` |
| C2 | probe-core, kinds | `internal/probe/**`, `internal/verify/**`, migration, `internal/store/check.go` |
| C3 | netgroup | `internal/store/netgroup.go`, `cmd/exocomp/netgroup.go`, `internal/reactive/checks.go` labels |
| C4 | mcp, api | `internal/mcp/**`, `internal/web/api_write.go` |
| C5 | aggregate | `internal/reactive/checks.go`, `internal/store/rule.go`, migration |
| C6 | apply | `cmd/exocomp/check.go`, `cmd/exocomp/netgroup.go`, `internal/probe/**` schema |

C0 is a natural single-wave parallel set (disjoint files). C2/C3 touch
`internal/reactive/checks.go`; sequence them or re-slice by region.

## 7. Sequencing

```
C0 (parallel) ─▶ C1 ─▶ C2 ─▶ C3 ─▶ C4 ─▶ C6
                         └▶ C5 (only if S9 is proven useful first)
```

C0 alone is a shippable improvement to the status quo and should land
regardless. C2 is the gating architectural decision. C5 is explicitly optional
pending S9.

## 8. Risks and kill criteria

- **D1 over-reach.** Unifying probes touches the gate path (production
  safety). Mitigate: keep `verify`'s public API and job routing frozen; the
  union is internal. Kill C2's breaking part and ship C0+C1+C3+C4 if the
  plan review finds the migration risk exceeds the S2 value.
- **Probe kinds without demand.** S2 is the proof; if cert-expiry is already
  covered by healthchecks.io, drop `tls`.
- **C5 is speculative.** The design deferred it for a reason. Prove S9 first
  or skip.
- **Scope creep into a full observability product.** Non-goal: retention,
  dashboards, alerting rules of our own. We emit gauges and events; Prometheus
  and the rules layer do the rest.

## 9. Open decisions for the operator

1. Which phases are in scope for the first program — C0 alone, C0+C1+C3+C4, or
   the full set?
2. D1/D2 (shared probe, breaking migration) now, or after C0 proves the demand?
3. Is S9 (aggregate / `--limit-hosts`) real, or do we kill C5?
