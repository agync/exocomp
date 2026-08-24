---
name: flake-triage
description: The single source of truth for this repo's known gate flakes, so every lane and integrator treats a `make check` failure identically. Use when a gate (commit hook, worktree gate, or manual `make check`) fails under concurrent load, to decide retry-once vs. real failure before reporting or masking anything.
---

# Flake triage

`make check` runs `fmt-check vet lint tidy-check test` (Makefile), and `test`
is `go test -race ./...`. A subset of these fail intermittently under
concurrent gating — several worktrees under `.worktrees/` running `make check`
at once — for reasons unrelated to the diff under test. This file is the one
place that enumerates them, so a flake is judged the same way in every lane and
by the integrator, not re-diagnosed per person.

## The rule

A failure matching a known flake below earns **exactly one** retry of the gate.

- Passes on retry → known flake, proceed.
- Fails a second time → **REAL**. Report it.
- Failure is anything not on this list → **REAL** on the first occurrence. Do
  not retry to see if it "goes away", and never relabel it a flake.

Retry the gate, not the one test in isolation: an isolated re-run hides real
failures that only appear under the full `-race ./...` load the gate imposes.

Masking a real failure as a flake is the worst outcome here — worse than a slow
gate. When unsure whether a failure matches, treat it as REAL and report it
with the output, rather than retrying.

## Known flakes

Each is a timing or shared-resource artifact of concurrency, not a defect in
the code being gated.

- **`TestTofuPlanRunsOfflineFromPluginCache`** (`internal/executor/tofu_offline_test.go`).
  Its online half warms `PluginCacheDir` by fetching the `hashicorp/local`
  provider before the offline half runs. Under concurrent load the warm-up
  races provider download/network and the child's `unshare -rn` namespace
  setup, so it flakes on first attempt.
- **serve integration timing** — the `TestServe*` / `TestDaemon*` cases in
  `cmd/exocomp/serve_integration_test.go`. They wait on daemon start-up, socket
  bind, and in-flight-job settling against real wall-clock deadlines that a
  loaded host blows through.
- **executor shell timing** — the `shell_*_test.go` group
  (`shell_group_test.go`, `shell_textbusy_test.go`) in `internal/executor`.
  These race process-group signal delivery, inherited-pipe draining, and the
  `ETXTBSY` retry of a just-written executable; under concurrent exec load the
  scheduler and the text-busy window stretch past what the assertions allow.
- **golangci-lint lock collision** — `make lint` fails with
  `parallel golangci-lint is running`. golangci-lint takes a machine-wide lock;
  when several worktrees hit the `lint` target at the same moment, all but one
  lose the lock. This is not a lint finding — the diff is clean.

## Fixed / root-caused

Previously flagged flaky here, now fixed. Listed so a future contributor does
not re-flag either as flaky or paper over a real regression as one.

- **`TestInterruptedListing`** (`internal/web`). Was a harness queue-runner
  race: a live runner could claim the `queued` row before the test transitioned
  it to `interrupted`. Fixed by creating the job via `store.CreateGatedJob`
  (lands in `awaiting_approval`, which no worker claims) then transitioning it
  to `interrupted` directly — race-free by construction, not a timing fix.
- **`TestConcurrentMigrationAppliesOnce`** (`internal/store/migrate_test.go`).
  Was not a product bug — a test-environment artifact of `make check`'s full
  `go test -race ./...` running every package's race binary at once, where
  SQLite's real write-lock handoff can exceed the production 5s
  `busy_timeout`. Fixed by giving the test its own 30s busy_timeout budget (via
  a factored `openDSN`; the production default stays 5s) and trimming its
  openers from four to two, matching the documented CLI+daemon scenario.

## Load ceiling

`make check` is ~2.5min solo but climbs to ~8-9min under several concurrent
lanes, and that saturation is what drives the flakes above. Keep concurrent
gates to **3-4 at most**. More lanes do not gate faster; they multiply
`SQLITE_BUSY`, lint-lock, and integration-timing flakes and slow every lane.

## Maintaining this list

These names are load-bearing. Before relying on this file, confirm a name still
exists (`grep -rn '<TestName>' internal cmd`); if a test was renamed, correct
the entry here rather than working around a stale name. Add a new entry only for
a flake proven unrelated to any diff — a genuinely flaky assertion in the code
is a bug to fix, not an entry here.
