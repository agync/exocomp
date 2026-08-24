# Knowledge base evolution log

Structural history of this bundle: pages added, deprecated, moved, merged, or split, and
corrections of fact. **This is not a software changelog** — releases, features, and bug
fixes belong to the project's release notes, not here.

Newest first.

## 2026-08-24 — Run-secret file staging moves to /dev/shm

The Phase 3 `--capture-file` design (bootstrap-first.md §2.3) staged a KUBECONFIG-shaped
run-secret to a `0600` file backed by a `noswap` `Tmpfs=` quadlet mount on
`/var/lib/exocomp/run`, asserting `statfs f_type == TMPFS_MAGIC` before writing and
refusing otherwise. That mount does not start at all under this project's one deployment
shape: podman refuses the `noswap` option on a rootless tmpfs mount ("only allowed with
rootful tmpfs mounts"), so the container failed to start with the mount in place — flagged
but held for the owner in the per-job-caps entry below, and now resolved by removing the
mount rather than the `,noswap` option.

Staging now targets `/dev/shm`: the RAM-backed tmpfs present unconditionally in every
rootless podman container, no mount or quadlet entry required. A memfd-backed alternative
was spiked first and rejected — real `nono` closes inherited file descriptors before exec,
and a memfd has no path a sandboxed child's `--read-file` grant can name, so the sandboxed
tier could never support it. `/dev/shm` has a real path, so the sandboxed drivers
(`sandboxedTofu`) now stage before building the sandbox profile and grant the path through
`SandboxProfile.ReadFiles` — proven against a real `nono` binary reading the staged secret
through its granted path, with an ungranted `/dev/shm` file denied as the control.

The staged file's bytes are mlocked best-effort (`secrets.LockBytes`/`UnlockBytes`, the
same fail-open, doctor-visible pattern the DEK's own lock uses) — there is no rootless
swap-avoidance option for tmpfs itself, so mlock is the only lever left, and a host that
forbids it still stages, unlocked, with the residual reported rather than the apply
refused. The file is unlinked once the subprocess that read it exits; a startup sweep
(`executor.SweepOrphanRunSecretFiles`, run once in `runServe`) removes any orphan a broken
cleanup path left behind. `internal/executor/tmpfs_linux.go`/`tmpfs_other.go` and the
`RunDirTmpfs` assertion are removed — nothing is left to fail closed on. The quadlet's
`Tmpfs=/var/lib/exocomp/run…` line is removed; the per-job run directory is ordinary
storage on the data volume again.

While fixing this, a latent aliasing bug surfaced in `Sandbox.Run`: it derives a per-call
profile from a shallow copy of `s.profile`, and `slices.DeleteFunc` zeroes a slice's
now-unused tail slots in place — without cloning first, that write landed in
`s.profile.Read`'s own backing array. A `*Sandbox` is reused across repeated `Run` calls in
the same job (the tofu driver's `init` then its operation), so a call whose working
directory happened to already carry a Read grant corrupted the *next* call's grant list
with a stray `""` entry. Fixed by cloning before the delete; a regression test pins two
sequential calls on one `*Sandbox` instance.

## 2026-08-24 — code review remediation: kill switch wired, devrun removed, setup honesty

Closing out the three companion code reviews (`.claude/inbox/2026-08-24-*`). Structural /
factual items not already logged by their own lanes:

- The approval page's "Silence & global kill" form was dead — its `POST /kill` target had
  no route. It now drives an operator-gated global silence (a `ScopeGlobal` `store.Silence`,
  the same mechanism the PAUSED banner already reads), not a maintenance cordon: a cordon
  stops schedules, which contradicts the form's own "Schedules still fire" copy, and is
  absent in the standalone web process. Silence pauses reactive-rule firing and is
  reversible.
- `devrun` (an ungoverned arbitrary-exec CLI verb with no sandbox and the full daemon env)
  is removed. Its enqueue-and-drain mechanism survives only as a test helper; the
  cross-process invariant tests that used it now drive the governed `run-cli` path instead.
- Setup's "nothing partial was left behind" promise is now true: a mid-`Commit` failure
  after `host.key` was written rolls the host identity back (`secrets.Rollback`), so re-run
  is possible and the message is honest.
- A post-bind web-surface serve failure is now fatal to the daemon (`serveUntilSignal`
  selects on the web error) instead of being silently swallowed while it prints "listening".

Info-level findings are dispositioned in
[security/review-2026-08-24-info-triage.md](security/review-2026-08-24-info-triage.md).

## 2026-08-24 — reactive: `approval link` needs its own --public-url

[operations/reactive.md](operations/reactive.md) previously documented `serve --public-url`
as what a decision link is rooted at without qualifying that this only holds for the daemon's
own push notifications. `approval link` re-derives a link in a separate CLI invocation and had
no way to read the running daemon's `--public-url` back (it lives in `reactive.Engine`'s
in-memory base URL, never persisted), so it silently printed the loopback address recorded at
daemon startup — unreachable off-box and stale after a restart. Correction of fact, plus the
command's own `--public-url` / `EXOCOMP_PUBLIC_URL` documented as the fix: pass the same value
`serve` was started with.

## 2026-08-24 — queue: enqueued set scoped to Drain (S2)

Simplification S2: the queue's in-memory ownership set (`remember`/`forget`/`owns`) exists
only to let a one-shot `Drain` claim and wait on the jobs it enqueued itself; `Run`, the
daemon's long-lived loop, claims any queued row regardless of who created it and never reads
the set. It carried one anyway — every idempotency-key hit `remember`ed the job it deduped
to, including one already terminal or already claimed elsewhere, and only a job this
instance actually launched itself was ever `forget`ten. A schedule re-firing the same key for
the life of the process leaked one entry per hit with no upper bound.

`WithoutDrainSet` is the new construction option a `Run`-only queue passes: it leaves the set
nil, `remember` becomes a no-op, and the daemon (`exocomp serve`'s queue) now carries no set
at all rather than one that only ever grew. Drain-only queues are unchanged — the default
still builds and populates the set, so claim filtering and the "every job I enqueued is
terminal" wait are exactly as before.

## 2026-08-24 — `internal/naming`: one path-component guard, enforced at creation

`executor.safeWorkspaceName` and `source.safeName` were independently-written copies of
the same rule (one safe filesystem path component), each reachable only as a
defense-in-depth check deep in a `filepath.Join` call — so a name that reached
`workspace add` or `source add` unvalidated still made it to disk. Both are replaced by
`internal/naming.Component`/`ValidateComponent`, a single leaf package with no internal
imports, and `workspace add` / `source add` now call it as the first thing they do,
before any store write or filesystem side effect. A traversal-shaped name (`../..`,
`a/b/c`, `.`, `..`, empty, NUL-bearing) is refused at the boundary; every downstream sink
keyed on a workspace or source name (module cache dir, plan `-out` path, the tofu→ansible
inventory handoff file, the git checkout root) is safe by construction rather than by
each site remembering to re-check.

The store layer itself was still asymmetric: `Store.CreateSource` had no first-party
validation of its own either, but `Store.CreateWorkspace` was the one with no guard at
all below the CLI boundary. It now calls `naming.ValidateComponent` too, so a future
non-CLI creation path can't hand an unsafe name to the plan `-out=` sink.

## 2026-08-24 — approval gate crash recovery consolidated

[architecture/reactive.md](architecture/reactive.md) gained a "Crash recovery of
the gate" subsection under Approvals. The per-state crash-recovery sweeps (a
queued/running interrupted-sweep, and the approval TTL sweep) left two seams
open: a job stuck `awaiting_approval` after a decided approval's release was
interrupted, and a gated job with no approval row at all. Both are now closed by
one idle-tick reconcile over every non-terminal job (`ReconcileNonTerminal`),
described there.

## 2026-08-24 — Schedule catch-up: dropped and doubled fires closed

Correction of fact in [operations/serve.md](operations/serve.md)'s "Catch-up after
downtime". Two claims there were only half-true. The section said the catch-up idempotency
key was anchored on `last_run` and that this covered "a restart between the enqueue and the
`last_run` stamp" — but the **normal** fire path carried no key at all, so a crash in that
window double-fired: the catch-up's `last_run`-anchored key never matched the un-keyed
normal fire. And `last_run` advanced unconditionally after the enqueue loop, so a fire whose
enqueue **failed** still stamped the schedule current and catch-up never re-fired it — a
transient error silently dropped a scheduled window. Both are closed: every fire (normal and
catch-up) now keys on the occurrence's own scheduled instant, so a fire and its catch-up of
the same occurrence dedup against each other; and `last_run` advances only once the
occurrence is safely enqueued, so a failed enqueue leaves it owed for the next catch-up.

## 2026-08-24 — threat-model: AI-11 auth timing equalizer removed (S1)

[security/threat-model.md](security/threat-model.md) revises the **AI-11** disposition. The
finding had two halves; the rate half (per-source / per-token fail counter that backs a
guessing run off before the KDF) is unchanged and remains the guard. The timing half — a
decoy argon2id the absent/inactive verify path paid so its latency matched a live row's — is
**removed** (simplification S1). Correction of fact: the equalizer never actually held (a live
row hashes at `argonTime=1` while the decoy PHC hardcoded `t=3`, leaving a ~3× gap), and the
oracle it targeted is accepted as out-of-threat-model for a single-operator homelab because
the leak is only which row _ids_ exist (not secret bytes of a 128-bit token) and enumerating
them still requires a guessing run the fail counter refuses pre-KDF. The verify path is now a
plain lookup: absent / inactive / mismatch all return unauthenticated with no decoy KDF; the
constant-time digest compare on a live row is untouched. `AI-11-resid` records the accepted
timing gap and names `internal/web/auth.go` as its owner.

## 2026-08-24 — per-job resource caps: cgroup v2 direct writes (byo-runner Phase 1)

The owner accepted per-job memory/CPU caps with the opt-in root-in-container privilege model,
so byo-runner Phase 1 is built — and built the way the real runtime allows, not the way the
design first sketched. `systemd-run` was ruled out on 2026-08-23 (the fedora-minimal image
ships no systemd); the shipped path is **cgroup v2 direct writes**
([design/byo-runner.md](design/byo-runner.md) §6 "Built", §9 Phase 1;
[operations/runners.md](operations/runners.md) gained a caps-enforcement section). The daemon
vacates its own cgroup (the v2 no-internal-process rule), enables the memory+cpu controllers,
and per opted-in job writes `memory.max` / `memory.swap.max=0` / `cpu.max` into a child cgroup
the job process is born into via `CLONE_INTO_CGROUP`. Opt-in and fail-closed: the default
rootless + `keep-id` deployment stays capless and refuses a capped job rather than running it
uncapped; `exocomp doctor` reports availability under "per-job caps".

Two corrections of fact from validating on real podman (5.8.4 host, 5.8.1 lima), not from the
design's theory:

- **`noswap` tmpfs is refused on rootless mounts** ("only allowed with rootful tmpfs
  mounts"). The Phase-3 run-dir mount in `packaging/quadlet/exocomp.container` carries
  `,noswap` under a rootless `UserNS=keep-id` container, so as written it would stop the
  container starting rootless. Flagged in the quadlet, held for the owner — not silently
  changed.
- **keep-id non-root cannot write `/sys/fs/cgroup`** even with `--systemd=always`, so the
  rootless-preserving delegation alternative does not work out of the box on podman 5.8.x.
  The shipped opt-in is root-in-container; the keep-id path stays unbuilt.

Enforcement was observed, not assumed: a 200 MB allocation under a 50 MB cap was OOM-killed
(`memory.events oom_kill 1`, SIGKILL → 137); a CPU workload under a 10% `cpu.max` throttled
(`cpu.stat nr_throttled 33`). The runner→job dispatch that would carry a stored runner's
`memory_max`/`cpu_quota` onto a job is a separate (queue/service) lane and is not yet wired;
`executor.ParseCaps` is the parsing seam it will use.

## 2026-08-24 — Cross-host fence and a cold-standby failover runbook

New page [operations/failover.md](operations/failover.md): the drilled cold-standby
procedure — restic restore, optional Litestream fast-forward, deliberate `serve`, and the
re-drive of interrupted work — plus the property behind it. The daemon now holds a
cross-host **active lease** (a single row in the store with a wall-clock TTL) above the
existing host-local `serve.lock` flock: the active daemon renews it, and a daemon starting
on another host that finds a live lease refuses to become active, naming the holder and its
last renew. A stale lease may be taken; force-taking a live one is the explicit
`--force-lease`. This closes split-brain across two hosts sharing one replicated store,
where the flock's pid-liveness is meaningless — notably the boot sweep, which is now run
only after the fence is held so a fenced-out standby never retires a live primary's jobs.
[operations/serve.md](operations/serve.md)'s "one daemon per data dir" section gained the
cross-host half.

## 2026-08-24 — Composed-chain `--require-approval` is now durably gated mid-run

[operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md) previously
documented a Phase-0 boundary: `--require-approval` was a **CLI confirmation** (`--assume-yes`),
not a durable per-step approval, because "the Sequencer releases a gated workflow step on its
own." Correction of fact: that gap is closed. A composed-chain step marked `require_approval`
is now held mid-run exactly as a single destructive apply is — the Sequencer arms a pending
approval (the same `CreateApproval`/`DecideApproval` capability path) and leaves the job in
`awaiting_approval` rather than releasing it, and `ClaimJob` refuses any queued job with a
pending approval so a running daemon cannot claim an unsettled step. `bootstrap`, `handoff`,
and `provision`'s `--require-approval` all gate their destructive apply this way; approve
releases exactly that step, reject blocks the rest. The bootstrap, handoff, and provision
operations pages were updated to describe the durable gate; the threat model records it as
AI-14. New migration `0023_workflow_step_require_approval.sql` adds the ledger flag.

## 2026-08-24 — threat-model records E6: DEK reseal is now crash-safe

[security/threat-model.md](security/threat-model.md) gained finding **E6** and its closure.
The E2 DEK rotation (recipient revocation) re-encrypted secret rows one at a time and flipped
`keyring.age` only after the loop finished, so a crash mid-loop stranded some rows under the
new DEK and the rest under the old while `keyring.age` still wrapped the OLD one — and nothing
read the staged `keyring.age.next` to reconcile, so the next daemon `Open` silently failed to
decrypt the already-moved rows. Correction of fact: the rotation is now staged-then-promoted
(the new keyring and metadata land beside the live files before any row moves; `keyring.age`
is renamed last as the single commit point) and a leftover `keyring.age.next` is reconciled to
completion on the next `Open` — loading both DEKs and re-sealing every row under the new one,
idempotently so repeated crashes converge. The `name|kind` AAD and every E1..E5 property are
unchanged. The residual E6-resid (a transit-sealed store cannot reconcile without the host
key) is recorded open.

## 2026-08-23 — machine-output.md's "read/list/status verbs support it" is now true

[operations/machine-output.md](operations/machine-output.md) claimed blanket `--output json`
coverage across read/list/status verbs while five held out as text-only: `mute list`,
`litestream status`, `seal status`, `handoff status`, and `hooks show`. Correction of fact:
the doc now names the five and their wire kinds explicitly, closed the same values-free way
as every other verb — `internal/wire` gained `mute.list`, `litestream.status`, `seal.status`,
`workflow.status`, and `hooks.show`, each a golden-tested projection that never carries a
secret value or a credential-bearing target URI.

## 2026-08-23 — Bootstrap captures survive a crash+resume

[operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md) gained a
resume-safety note: a bootstrap play the daemon re-arms after a mid-run crash keeps its
`--capture`/`--capture-file` declarations and seals under the same `ab:<workflow>`, so the
apply's binding is never left empty by the resume. This corrects the earlier implicit gap —
the capture wiring rode the ExtraVars map under reserved keys, which the sequencer's re-arm
did not copy, so a resumed play sealed nothing. The wiring is now a first-class job field the
re-arm carries; behaviour of `--capture` and `--capture-file` is otherwise unchanged.

## 2026-08-23 — four pages and one comment caught up to the stdio removal

[security/threat-model.md](security/threat-model.md), [architecture/overview.md](architecture/overview.md),
[operations/index.md](operations/index.md), and [operations/machine-output.md](operations/machine-output.md)
still described the two-transport world after the stdio MCP server's removal. Corrections of
fact: the AI-1-resid threat-model residual is now recorded CLOSED (the stdio face is gone
entirely, not just its decision tools, so the token-scoped transport the residual called for
is what shipped); the architecture overview's face-mapping table no longer names `exocomp mcp`
as a face that enqueues, crediting MCP-over-HTTP as the agent-aware wrapper over `/api/v1`
instead; the operations index's two link descriptions drop the stdio mention; and
machine-output.md's "third face" section is rewritten around the one HTTP surface, the CLI as
the local agent path, and the npx http-to-stdio bridge, cross-linking [operations/mcp.md](operations/mcp.md)
rather than duplicating it. `internal/web/server.go`'s `mcpSrv` field comment is corrected to
say it serves `POST /mcp`, not "the same package `exocomp mcp` serves over stdio".

## 2026-08-23 — `bootstrap run --capture-file`: KUBECONFIG file staging (Phase 3)

[operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md) gained the
**`--capture-file`** surface, and [design/bootstrap-first.md](design/bootstrap-first.md) is
marked **Phase 3 landed** with §2.3's open owner-decision resolved. `--capture-file NAME=FILE`
seals a play-fetched value exactly as `--capture` does, but the apply receives it as a **file
path** — the daemon writes the sealed bytes to a `0600` file in the tofu run's private tree and
sets `NAME` to that path — for a tool (`kubectl`/`helm`, exec-auth) that wants a `KUBECONFIG`
file rather than decomposed env fields (Option **C-file**). The owner approved C-file on the
hard condition that the staged secret never touches durable disk: the quadlet mounts a `noswap`
tmpfs at `/var/lib/exocomp/run`, and the daemon asserts `statfs f_type == TMPFS_MAGIC` on the
per-job run dir and **refuses to stage (failing the apply) when it is not** — the
never-silent-downgrade discipline (ADR 0005) applied to disk (agentfs/CoW was evaluated for the
backing and rejected in favour of plain tmpfs). `exocomp doctor` now reports the run dir's
backing under **job run dir**. Env-decomposition (`--capture`, Phase 2) is unaffected and needs
no tmpfs.

**Correction of fact.** The design page's Status said "Design only — no product code lands with
this document." Phases 0–3 have since shipped, so the Status now records them as landed and
points at §3 for what each touched; §2.3, which "presented both and did not choose," now records
the owner's decision (C-env first, then C-file).

## 2026-08-23 — the stdio MCP server is gone; one agent surface remains

[operations/mcp.md](operations/mcp.md) no longer documents `exocomp mcp`: the bespoke stdio
MCP server and its `LocalCaller` (ambient local-operator authority, no credential, no scope)
are removed from `internal/mcp` and `cmd/exocomp`. `POST /mcp` on the `exocomp serve
--web-addr` listener — bearer-authenticated, ADR-0009 scoped — is now the one MCP surface. A
stdio-only MCP client bridges to it with an off-the-shelf http-to-stdio wrapper (`npx
mcp-remote`, supergateway); the sanctioned LOCAL agent path is the agent-aware CLI
(`--output json`, exit codes), not an MCP hop. `HandleMessage` and the HTTP transport
(`internal/web/mcp_http.go`) are unchanged; every test that exercised shared dispatch
behaviour through the old stdio `Serve` loop now drives it through `HandleMessage` directly.

## 2026-08-23 — ADR-0014 reconciled to the single MCP-over-HTTP surface

[adr/0014-agent-facing-surface.md](adr/0014-agent-facing-surface.md) and its design doc
[design/agent-interface.md](design/agent-interface.md) were reconciled to the decided
architecture. The open governance question — ambient stdio authority vs a uniform token — is
resolved: exocomp maintains exactly one programmatic MCP surface, MCP over HTTP with an
ADR-0009 bearer and the readonly/operator/approve scopes, mounted on the `/api/v1` listener; no
maintained stdio server and no ambient-authority path. The local agent path is the agent-aware
CLI; a stdio client bridges to `POST /mcp` with an off-the-shelf http-to-stdio wrapper.
Corrections of fact: the remote selector is `--remote` / `EXOCOMP_REMOTE` (not `--host`), and
`internal/service`, the write `/api/v1`, remote CLI client mode, and the HTTP MCP server have
all shipped — the ADR now ratifies built reality, with the bespoke stdio server's removal as
the one follow-on code change it ratifies. Status stays Proposed (ratification is the owner's
act).

## 2026-08-23 — bootstrap-first B1 example fixed; lane skills updated

[design/bootstrap-first.md](design/bootstrap-first.md)'s Option B1 signature no
longer shows `--capture` on the base command — that flag belongs to Phase 2,
per the phased plan in §3; Phase 0 ships with no capture, matching what
shipped. Also folded real lessons from this session's lane dispatch and
integration work into `.agents/skills/lane-brief-authoring` (worktree created
before dispatch, absolute-path-only writes, integrator dispatched only after
WORKFLOW-COMPLETION, migration numbers final only at merge) and
`.agents/skills/lane-integration` (run `make check` to completion in the
integrator's own turn; stop and report on an "untracked working tree files"
fast-forward refusal), and sharpened the dangerous-vs-confusing read-misread
line in `.agents/skills/mcp-comprehension-audit`'s scoring rubric.

## 2026-08-23 — `bootstrap run --capture`: the ansible→tofu env handoff (Phase 2)

[operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md) gained the
**`--capture`** surface, closing the loop [design/bootstrap-first.md](design/bootstrap-first.md)
opened. A bootstrap play `fetch`es a sensitive value into the run's capture dir; the daemon
seals it in-process under `ab:<workflow>`, and the `tofu apply` of the same chain resolves that
binding and receives the value as env — the mirror of the tofu-side `tf:<workspace>` auto-bind.
This is the **env-decomposition** path (Option C-env): the value reaches the `kubernetes`/`helm`
providers as `KUBE_HOST` / `KUBE_TOKEN` / … with nothing staged to a filesystem. The scoping is
the binding string, so an unrelated tofu run inherits nothing; a capture whose name is not a
POSIX identifier is sealed-but-unbound with a reason, never mangled; `--capture` and
`--run-secrets` are mutually exclusive (one apply binding slot).

**Correction of fact.** The design's §2.2 (Option B1) sketched the flag as
`--capture k=fact|@file` on the Phase-0 verb signature. As built, `--capture` is a **Phase-2**
flag and takes **`NAME=FILE`** only — a play-fetched file, no registered-`fact` source (Phase 1
built the fetched-file capture; the fact-source shape in §2.1 was not implemented). Code wins;
this records the divergence rather than editing the Proposed design page.

## 2026-08-23 — MCP handshake gains an event-family glossary

[operations/mcp.md](operations/mcp.md)'s handshake section notes the `initialize` result's
new `instructions` string (MCP spec's server-instructions field): a 1-2 sentence glossary
distinguishing `get_job_events` (a job's own execution log) from `list_events`/
`explain_event` (inbound reactive trigger events) — two unrelated id-spaces that share the
word "event" and that a cold agent otherwise conflates. No tool or field renamed.

## 2026-08-23 — schedules catch up after downtime

[serve.md](operations/serve.md) gained a **Catch-up after downtime** subsection. The
scheduler now records each schedule's `last_run` (migration 0021) and, on (re)load, fires
one coalesced catch-up for a schedule whose cron had an occurrence between `last_run` and
now — closing the forward-only gap where a backup or restore drill scheduled during a
daemon outage was silently dropped. Documents the coalesce (one fire, not one per missed
occurrence), the never-fired no-back-fire rule, and the `last_run`-anchored idempotency
key.

## 2026-08-23 — `bootstrap run`: the ansible-first chain (Phase 0)

**New page.** [operations/ansible-first-bootstrap.md](operations/ansible-first-bootstrap.md) —
the `exocomp bootstrap run <playbook> <workspace>` verb, Phase 0 of
[design/bootstrap-first.md](design/bootstrap-first.md). It composes `[ansible-play, tofu-apply]`
on the existing Sequencer — the handoff mirrored, play first — with no credential capture
between the steps: the apply reads its own pre-existing `--run-secrets` binding, and any
play-produced credential is handed over manually until the Phase-1 capture hook lands.

Two spec notes for a future editor. First, the verb takes `--inventory` because the play runs
before any apply, so there is no `tf:<workspace>` inventory to build its hosts from — a
difference from `handoff run`, whose play consumes the apply's output. Second,
`--require-approval` is a **CLI confirmation** (refuse the destructive chain without
`--assume-yes`), not a durable per-step approval: the Sequencer releases a gated workflow step
on its own, so a runtime mid-chain approval for a composed chain would need Sequencer support
and is out of Phase-0 scope. The design's Phase-0 section did not enumerate `--inventory`,
`--require-approval`, or `--output json`; they are additive here and this note records why.

## 2026-08-23 — bootstrap-first design page (proposed)

**New page.** [design/bootstrap-first.md](design/bootstrap-first.md) — a Proposed design for
the mirror of the provision-first handoff (items A/D): an ansible-first-then-tofu chain for
the Kubernetes persona (ansible bootstraps kubeadm nodes → tofu manages cluster resources).
It names three confirmed gaps — no ansible-output→seal capture, no ansible-first chain
exposed (though the Sequencer is order-agnostic), and env-only run-secrets vs a `KUBECONFIG`
file — and specifies each fix as the mirror of a tested tofu-side feature. The KUBECONFIG
file-vs-env question is left an open owner decision. Linked from
[design/v1.1-roadmap.md](design/v1.1-roadmap.md) as a proposed follow-on. Design only; no code.

## 2026-08-23 — the daemon's apply seals tofu outputs too

**Correction of fact.** [sealed-tofu-outputs.md](operations/sealed-tofu-outputs.md) and item D
of [v1.1-roadmap.md](design/v1.1-roadmap.md) said the daemon's apply captures the inventory but
does not seal sensitive outputs, because its `TofuOptions` left `OutputSealer` unset. It now
sets the same `executor.EnvelopeOutputSealer` the daemonless `workspace apply` path does, so a
scheduled, reactive, provision-chain, or serve-up apply binds `tf:<workspace>` identically to a
CLI-inline one. Item D is landed on every apply path; the caveat is gone from both pages.

## 2026-08-23 — the `cli` kind routes through a running daemon

`exocomp serve`'s kind router served only `tofu` and `ansible`; a queued `cli` job fell
through to the scheduler's shell/backup driver, which refused any kind it did not
recognize. `run-cli` worked around this by refusing outright whenever a daemon was up, and a
`--require-approval` cli job that *was* approved died on release with `sched: no driver for
kind "cli"`.

A `cliJobDriver` alongside `workspaceDriver`/`ansibleJobDriver` closes both: it rebuilds
`executor.CLIOptions` from the allow-list entry the job's argv[0] names — the same per-job
rebuild those siblings already do for a workspace's state key and a play's vault password —
and fails closed, never falling back to a raw binary, when the entry was deregistered
between enqueue and claim. `run-cli` now enqueues for a running daemon and follows the job
it creates, the shape `workspace apply`/`playbook run` already have.
[operations/cli-run.md](operations/cli-run.md).

## 2026-08-23 — sealed tofu outputs get an operations page

**New page.** [sealed-tofu-outputs.md](operations/sealed-tofu-outputs.md) — the credential
half of the Terraform→Ansible handoff, which had shipped as a capability with no page: a
`sensitive = true` output sealed as `tf-output/<workspace>/<output>` and bound under
`tf:<workspace>` to the output name upper-cased, resolved automatically by any run that
targets that workspace's standing inventory and by nothing else. Documents the three
deterministic names, the auto-versus-flag decision and its scoping rule, the two cases where
an output is sealed but refused a binding, and the honest limit that the daemon's apply does
not set the sealer yet.

**Corrections of fact.** [terraform-ansible-handoff.md](operations/terraform-ansible-handoff.md)
said a sensitive output "is dropped before anything is written or logged". It is never
written or logged, but it is no longer dropped: on an apply path that wires the sealer it is
sealed and bound. [provision-converge.md](operations/provision-converge.md)'s standing-
inventory section gains what naming `tf:<workspace>` now also resolves, and
[v1.1-roadmap.md](design/v1.1-roadmap.md)'s combined TF+Ansible table marks item D landed
with its remaining daemon-side wiring named.

## 2026-08-23 — agent-interface docs debt closed: remote CLI, API, machine output, runners

**Four new operations pages.** [remote-access.md](operations/remote-access.md) — `exocomp
login`'s TOFU certificate pinning, `--remote`/`EXOCOMP_REMOTE`/`--token` precedence, and the
honest list of which verbs the CLI actually remotes today (jobs/approvals/sources/
workspaces/doctor, reads only — writes exist on the wire but the CLI's client mode does not
call them yet). [api.md](operations/api.md) — the `/api/v1` surface as shipped: both the
read routes and the write routes (`internal/web/api_write.go`) are live, not deferred as an
older file comment in that package still claims; the wire envelope, Idempotency-Key dedup,
and the error-code-to-HTTP-status table. [machine-output.md](operations/machine-output.md)
— `--output json`/`EXOCOMP_OUTPUT`, the envelope, the error taxonomy, and a correction: the
exit-code table `internal/wire` defines (0-7) is only partly wired to the process —
`main.go` exits 1 on any error without inspecting the taxonomy code, so only 0/1/2 (the last
only on specific commands) are reachable today; codes 3-7 exist in the JSON envelope and in
the remote client's `APIError.ExitCode()`, which nothing calls yet. Also covers `exocomp
mcp` briefly. [runners.md](operations/runners.md) — the `runner add/list/show/rm/import`
registry, stated plainly as definitions only: nothing builds an image or runs one yet. All
four are linked from the operations index.

[sources.md](operations/sources.md) gains `secrets rotate`/`rm`, `--generate`, and
`--expires`, and the expiry semantics a migration comment already stated but no operator
page did: expiry is a hygiene marker only and never gates decryption or run-secret
injection — an expired secret still opens and a run still gets it, with a warning logged
naming the secret (never the value).

**Corrections of fact.** [architecture/overview.md](architecture/overview.md)'s Components
table said `internal/service` already owns "all business logic." It owns four write verbs
(`PlanWorkspace`/`ApplyWorkspace`/`RunPlaybook`/`DecideApproval`) and their resolvers, which
every write face now routes through — but every read still goes store-direct. The table
also separates the loopback tofu state backend (`internal/api`) from the operator
`/api/v1` surface (`internal/web`), previously conflated as one row.
[v1.1-roadmap.md](design/v1.1-roadmap.md)'s near-term table still priced gaps A and B as
future work after its own "Landed since v1.0" section said they had landed; the table now
marks A, B, and C (ansible drift parity, shipped this wave —
`internal/reactive/drift_ansible.go`) landed with pointers, leaving D open.
[design/byo-runner.md](design/byo-runner.md) §6 proposed `systemd-run --user --scope` for
per-job resource caps without checking it against exocompd's own deployment: validated
against the packaging tree, the wrapper works unmodified only on a bare host — the shipped
container has no `systemd` binary and is not started with cgroup delegation, so the
in-container path is cgroup v2 direct writes gated on a privilege-model decision (root vs
keep-id+delegation) left for the owner. §9 Phase 1 is marked blocked-as-written for the
packaged deployment accordingly.

## 2026-08-23 — write attribution closes the token-audit gap

A remote write (`workspace plan`/`apply`, `playbooks/run` over the API) now leaves a
durable record of which token asked, on the same append-only `job_event` ledger ADR
0007 already treats as the audit surface — no new column. `internal/web/api_write.go`
resolves the caller's identity and passes it to the `internal/service` verbs as an
`Actor`, which append a `system` event (token name only, never its secret) before the
job is claimable; a gated apply's pending approval carries no field of its own, so the
same event on the job its `JobID` points at is the attribution record. An empty
`Actor` — every CLI call today — records nothing, unchanged from before.

## 2026-08-23 — the config-drift sweep landed

**Correction to the entry below.** [architecture/reactive.md](architecture/reactive.md)
said drift was a `tofu-plan` schedule's story; the sweep now covers both planes, and the
Drift section records what changed. An ansible `--check` job whose recap reported changed
tasks raises an `AnsibleDrift` event beside `TofuDrift`, pages on a stable identity, and is
auto-closed by a later clean check — the step the entry below called remaining.

The page also records the one thing the two planes do not share: what a drift subject is.
Tofu keys by workspace; ansible has no such noun on the job row and keys by
`<runbook>/<playbook base name>`, because the manual `playbook run` verb stamps every job
with the same runbook.

## 2026-08-22 — playbook run flags and config-drift parity

**New page: [operations/playbook-run.md](operations/playbook-run.md)** — `playbook run`
gained the ansible passthrough flags (`--limit`, `--tags`, `--skip-tags`, `--check`,
`--diff`) and a repeatable `-e/--extra-var` taking `key=val` or `@file`. The page records
where extra-var values live: a `key=val` is carried on the job row and written to a
run-private file, never onto ansible's own argv (the vault password and run-secrets keep
their existing out-of-argv paths); `@file` passes only the path. It documents the trust
posture — a `-e` value is operator run-config, not an envelope secret; use a run-secret or
`--vault-project` for sensitive values.

**Config-drift parity (roadmap C).** `playbook run --check` now surfaces the check-mode
drift outcome the executor already parsed: a job event whose `raw_json` carries
`ansible_check` with per-host would-change counts, and a human `ansible check: N clean,
host X: M would change` line. `schedule add --kind play --check` registers the config-drift
schedule — check-mode plays on a cron against a standing inventory, the config twin of a
tofu-plan drift schedule. The operations index lists the new page. The reactive seam
(`executor.AnsibleCheckChanges`, mirroring `TofuPlanChanges`) is in place for an
`AnsibleDrift` sweep to mirror `SweepDrift`; wiring that sweep in `internal/reactive` is
the remaining step.

## 2026-08-22 — provision → onboard → converge, and the standing inventory

**New page: [operations/provision-converge.md](operations/provision-converge.md)** —
`exocomp provision run` chains a `tofu apply` to the hosts its declared outputs name:
a bounded wait for their SSH, the seeded onboarding runbook (host key pinned first,
then fleet key and automation user), and the configure play, as one four-step
step-ledger workflow. The page also covers `tf:<workspace>` as a STANDING inventory a
`schedule --kind play` targets by name, and the fail-closed rule that a hostless or
stale inventory runs no play at all.

[terraform-ansible-handoff.md](operations/terraform-ansible-handoff.md) keeps
describing the two-step `handoff run` and now points at the new page for the case
where the apply creates hosts that do not exist yet; the operations index lists both.

**Correction of fact.** `exocomp host onboard` used to print its onboarding command
with `-e exocomp_fleet_pubkey="<key>"`. Ansible's `k=v` extra-var form splits on
whitespace and an `authorized_keys` line has two spaces in it, so that command set
`exocomp_fleet_pubkey` to the key *type* alone. It now prints — and the chain passes —
the JSON `--extra-vars` form.

## 2026-08-22 — M3c maintenance: lifecycle wiring, deps, packaging, docs

The maintenance pass over merged M3c hardens the seams and links the operator pages the
pass-2/pass-3 lanes had left unlinked.

**The operations index now links the M3c pages** that shipped without an index row:
[run-lifecycle.md](operations/run-lifecycle.md) (pre/post-run hooks and the health gate),
[terraform-ansible-handoff.md](operations/terraform-ansible-handoff.md) (the story-5
apply→play chain), and [upgrade.md](operations/upgrade.md) (upgrade/rollback as a ledger
choreography). [plan-policy.md](operations/plan-policy.md) was already linked.

**A source's lifecycle now applies to `playbook run` and `handoff run`, not just `run`.**
[run-lifecycle.md](operations/run-lifecycle.md) records that `playbook run --source` and
`handoff run --source` compose the same `[pre_run] → main → [post_run] → gate` workflow via
the one shared composition; a source with no `hooks.yml` leaves either command unchanged.

**A source tree must stay outside the data dir.** [sources.md](operations/sources.md) gains
the boundary: the sandbox denies the data dir (`exocomp.db`, `host.key`, `keyring.age`) to
every job, so a source path that is an *ancestor* of the data dir cannot be granted and the
run fails to start. Keep playbooks and workspaces in their own subtree.

**`pyproject.toml` is parsed with a real TOML parser** now (`github.com/BurntSushi/toml`),
replacing a line scanner. [ADR 0011](adr/0011-project-dependency-resolution.md) records
which tables are read — `[project].dependencies` and `[dependency-groups]` — and that a
PEP 735 `include-group` is skipped, `optional-dependencies` are excluded, and a broken file
fails the vendoring step rather than resolving to fewer deps.

**The toolchain pins are committed at `packaging/MANIFEST.json`**, the explicit source of
the versions rather than a constant compiled into the binary. The image build copies it in
(filling only the Fedora-resolved `git` version) and sets `EXOCOMP_TOOLCHAIN_MANIFEST` so
the daemon reads the shipped pin; [bill-of-materials](sbom/bill-of-materials.md) records the
wiring and a packaging test guards against drift from the Containerfile args.

**Backup quiesce hooks landed** in `internal/backup`: a backup can carry pre/post quiesce
steps (reusing the `internal/hooks` shape) so a live-DB snapshot is application-consistent —
a failed pre-quiesce aborts the backup before any snapshot is taken (never a torn snapshot),
and the un-quiesce always runs once the source was quiesced.

## 2026-08-22 — M3c plan policy: one operator page added

[operations/plan-policy.md](operations/plan-policy.md) is new: the CEL gate over a
workspace's `tofu show -json` plan. It documents the `plan` variable and the plan-show
keys a policy reaches (`resource_changes[]`, `resource_drift[]`, `.change.actions[]`),
compile-at-add refusal, the two actions (`deny` sealing a plan unconsumable so its apply
refuses, `require_approval` routing the apply through the existing gate), the operator
transcripts for each, and the boundaries: the verdict record carries only policy name and
action, evaluation re-describes the sealed plan offline from the plugin cache with no
backend or bearer, a workspace with no policies pays nothing, an unexaminable plan seals
fail-closed denied, and a raw plan-file apply is not gated. Linked from the operations
index between reacting-to-alerts and sources.

## 2026-08-22 — M3a reactive: two pages added, overview's carve-out closed

[architecture/reactive.md](architecture/reactive.md) is new: the webhook ingress and its
derived per-source path token, the one normalized event shape and why its dedup key
excludes the status, the sandboxed CEL environment and its time and cost budgets, the
guardrails in ledger order with the reason strings each writes, the approval token's
derivation and its mapping onto the six points of the approval surface contract, and
drift-as-an-event with the event table as its own checkpoint.

[operations/reactive.md](operations/reactive.md) is new: wiring Alertmanager,
healthchecks, and generic JSON senders; the sealed-secret names the ntfy transport reads
and why a bare public topic is refused rather than warned about; `rule`, `silence`,
`approval`, and `event` usage; and the `tofu-plan` schedule kind. Linked from
`architecture/index.md`, `operations/index.md`, and the top-level `index.md`.

[architecture/overview.md](architecture/overview.md) gained the paragraph closing the
other half of the one-queue carve-out: guardrails and approvals are enforced at enqueue,
and `awaiting_approval` is a state no claim statement admits rather than a UI convention.
## 2026-08-22 — M3a security folds: keyring generations, manifest tags, kit CLI

The five convergent findings of [design/user-feedback.md](design/user-feedback.md) that
touch keys and backups are implemented; the pages describing them are corrected here.

**The keyring generation lives beside the keyring, not inside it.** The design called for
a versioned keyring file. age rejects any framing added before or after an armored file —
verified against the `age` CLI both ways — and the printed card's
`age -d -i recovery.key keyring.age` is a CI-executed build artifact, so the counter rides
in `keyring.version` next to `keyring.age`: in the data dir, in the backup staging tree,
and at every target root. A directory with no such file is generation 1, which is every
data dir written before this.

[operations/disaster-recovery.md](operations/disaster-recovery.md) gains the two files the
staging tree grew (`keyring.version`, `MANIFEST.mac`), what a restore does with them —
authenticity before integrity, tampering reported apart from corruption, an untagged
snapshot restored with a loud warning — and a "Deriving without the daemon" section for
`exocomp kit derive`, `kit verify` and `identity migrate`.

**The card takes the design handoff's section layout**, numbered 1-6 with the
hand-written storage-credential box, the kit version line and the page digest. The
handoff's section 7 (dead-man ping) is *not* rendered: nothing configures a dead-man ping
yet, and a card section describing an unbuilt feature is the failure mode the M1 gate
corrections were about. It lands with the reactive layer. The derivation block renders
from the salt and purpose table in code, as the design-handoff entry below records, and
every command renders as `exocomp` ([ADR 0002](adr/0002-project-name-exocomp.md)).

[architecture/execution.md](architecture/execution.md)'s OpenTofu section states that a
saved plan is encrypted at rest to the keyring's recipients and decrypted into the run's
private directory for an apply or an approval diff. Plan artifacts are not part of the
backup staging tree, so the restore drill is unchanged.

The repository root gains a [DCO](../DCO); [AGENTS.md](../AGENTS.md) records that
contributing certifies it and that no sign-off trailer is used, invariant 9 admitting
none.

## 2026-08-22 — M2d reconciliation: serve, execution, and BOM corrected against code

[operations/serve.md](operations/serve.md): the daemon-aware CLI list gained `backup
run` and `playbook vendor`; the single-instance probe is now documented as shared and
non-blocking, with the acquire-side retry that keeps a probe from costing a starting
daemon the directory; startup's stale-lock warning and a "Known limits" section
(`SQLITE_BUSY` on the WAL pragma, the executor's ~2s pipe-drain grace) are new; a
"Schedules" section documents `--cron` validation and skip-on-reject reload behaviour.

[architecture/execution.md](architecture/execution.md): a "Queue semantics" section
states the one-shot-drain/no-daemon-rows rule and that `store.ClaimJob` enforces
concurrency keys in one SQL statement, not an in-process map. The executor tier table
now says what `local` actually ships (no per-job tmpfs, no separate executor UID) and
marks `local-sandboxed`/`container` designed for M3, with the trust-equivalence
consequence stated plainly. The stale-tflock paragraph flipped from aspiration to
implemented (`workspace unlock`, audited system event).

[adr/0004](adr/0004-opentofu-http-state-backend.md) gained a status note dating the
stale-lock recovery landing. [operations/disaster-recovery.md](operations/disaster-recovery.md)
re-dated its two lingering "M2" failure-mode rows to M3, neither matching a v1.1
backlog item. [sbom/bill-of-materials.md](sbom/bill-of-materials.md) refreshed
drifted Go module rows against `go.mod` and closed the ansible-runner
re-evaluation: staying with the direct driver, JSON callback plugin backlogged v1.1.

[design/delivery.md](design/delivery.md) marks v0.5/M2 landed 2026-08-22
(`test/e2e/m2-gate.sh`), splitting the full 8-screen TUI and wizard out to v1.0/M3
now that both are designed.

## 2026-08-21 — M2c: serve page added, backup run states corrected

[operations/serve.md](operations/serve.md) is new: `exocomp serve`'s flags
(`--port`, `--drain-grace`, `--reload-interval`), the signal contract for `SIGHUP` and
`SIGTERM`/`SIGINT`, the single-instance lock on `<data dir>/serve.lock`, and what
`workspace plan|apply` and `playbook run` do while a daemon is up — they enqueue and
follow the job instead of draining a queue of their own. Linked from
`operations/index.md` and the top-level `index.md`.

[operations/disaster-recovery.md](operations/disaster-recovery.md) gained the terminal
states a `backup_run` row can hold. `cancelled` is new — a leg stopped by a shutdown was
previously indistinguishable from one the target failed, and a row left `running` now
means only that the writing process was killed outright. Its failure-mode table said
twice that "`exocomp serve` currently serves the OpenTofu state backend and nothing
else"; that stopped being true when serve became the daemon, and both parentheticals are
gone.

## 2026-08-21 — M2b caches: offline-caches page added, MinIO assessment closed out

[operations/offline-caches.md](operations/offline-caches.md) is new: `playbook vendor`
and the ansible collections cache, the tofu plugin cache and its `OfflineProviders`
mode, and how to verify offline resolution for real. Linked from
`operations/index.md` and the top-level `index.md`.

[operations/disaster-recovery.md](operations/disaster-recovery.md) gained the operator
commands for sealing a `restic_s3` target's S3 credentials
(`s3/<target>/access-key-id`/`secret-access-key`) — the row naming and fallback
behavior were already documented, but not how an operator actually seals one; the
backup lane shipped the mechanism without that step.

[sbom/minio-assessment.md](sbom/minio-assessment.md)'s decision section is no longer a
backlog item: the S3 test fixture (`internal/backup/s3_test.go`,
`test/e2e/m1-gate.sh`) is SeaweedFS, verified against the real restic leg and this
project's own SigV4 client, with the full e2e gate passing against it. Two
fixture-only quirks the swap surfaced are recorded there: SeaweedFS rejects a signed
request without a real `-s3.config` identity (anonymous mode is not viable for
restic), and the mounted config needs world-readable permissions because rootless
podman's UID remap does not preserve ownership into the container.

## 2026-08-21 — M2a scheduler closes half the M1 backup carve-out

`architecture/overview.md`'s "one queue, no side doors" section named `exocomp backup run`
as a direct CLI invocation outside the queue, "because there is no scheduler yet to enqueue
[it] against." `internal/sched` now exists: it fires cron-scheduled jobs through
`(*queue.Queue).Enqueue`, the same path everything else uses, so the reason the carve-out
gave no longer holds for backups. The paragraph now says what `backup run` and
`schedule add --kind backup` actually do — one queued job per target, concurrency key
`backup:<target>` — and narrows to `exocomp restore`, which stays a direct invocation:
restoring is an operator action at a cold start, not something the scheduler reaches.
## 2026-08-21 — M2a hardening lands git chunking and sealed S3 credentials

`operations/disaster-recovery.md`'s leg table previously flagged the git leg's 90 MB
chunking and 4 GB repo-size guard as not-yet-shipped M2 work (see the M1 gate corrections
entry below). Both now ship in `internal/backup/chunk.go`, so the row is corrected to
describe the shipped mechanism instead of its absence. The S3 leg row gains the sealed
credential resolution added alongside it: `s3/<target>/access-key-id` and
`.../secret-access-key` rows, process-environment fallback with a logged warning. A new
line documents `exocomp ssh-serve`, which was not previously mentioned anywhere in the
bundle.
## 2026-08-21 — M2a tofu corrections

Building the OpenTofu half found three claims in
[ADR 0004](adr/0004-opentofu-http-state-backend.md) the shipped code does not
support (code wins, per [AGENTS.md](../AGENTS.md)):

- **Bearer scope.** The ADR said the backend bearer is "valid only for that job's
  lifetime". It is derived per workspace and is therefore stable. A per-job token
  cannot work as written: a saved plan file carries the bearer it was made against
  and is applied by a later job.
- **Drift representation.** The ADR said exit 2 sets `job.drift = true`. No such
  column exists, and `executor.Result` carries one exit code that the queue reads
  as the job's verdict, so the driver records the plan outcome in the job's
  append-only `job_event` log and reports exit 0. The plan job succeeds.
- **Retention pruning** of old `tfstate` serials was stated as a property. Every
  serial is kept; nothing prunes them yet, and the page now says so.

Added to the same ADR's consequences: a saved plan file is a credential, because it
carries the backend address and bearer, which is also why the backend address has to
survive between a plan and the apply that consumes it.

`architecture/execution.md` gained an "OpenTofu runs" section describing the shipped
driver: the generated backend override, the private `TF_DATA_DIR`, the `TF_ENCRYPTION`
passphrase, the environment allowlist, and why two runs cannot share a workspace
directory.

Two rows of `operations/disaster-recovery.md` said `exocompd serve` "is not yet
implemented", which the M1 corrections had made true. `exocomp serve` now exists and
serves the state backend; the startup integrity gating, the single-recipient nag, and
the bootstrap wizard those rows point at are still unbuilt, so the rows name that
instead of the command.

## 2026-08-21 — Bundle created

The `docs/` bundle was created as an OKF v0.2 knowledge base, curated from the
pre-repository design archive that produced the project. Nothing existed before this entry.

**Added — sections and pages**

| Path | Contents |
|---|---|
| [index.md](index.md) | Root index and section map |
| [log.md](log.md) | This log |
| [adr/](adr/index.md) | Index plus ADRs 0001–0006, all Accepted |
| [architecture/](architecture/index.md) | Index, `overview.md`, `platform-contract.md`, `execution.md` |
| [design/](design/index.md) | Index, `requirements.md`, `amendments.md`, `delivery.md` |
| [operations/](operations/index.md) | Index, `bootstrap.md`, `disaster-recovery.md` |
| [sbom/](sbom/index.md) | Index, `bill-of-materials.md` |
| [history/](history/index.md) | Index plus 13 provenance records imported byte-identical |

**Sources.** The authoritative source was the archive's accepted technical design document.
Supporting sources were eight research notes, a post-decision addendum, three candidate
designs, and a 30-gap adversarial critique. The curated pages are summaries; the archive
documents themselves were imported unchanged into `history/`.

**Naming.** Most of the archive predates the rename from the working name "control" to
**exocomp** ([ADR 0002](adr/0002-project-name-exocomp.md)); the post-decision addendum was
written after it and mixes both names. Curated pages use the current name throughout.
Imported history pages were deliberately **not** rewritten — the reasoning is stated in
[history/index.md](history/index.md).

**Import fidelity.** All 13 history files are byte-identical to the archive originals. They
were checked for absolute filesystem paths, home directories, and local file URLs before
import; none were present, so no substitutions were needed. The archive's `design/` and
`research/` split is preserved under `history/` so the one relative cross-reference in the
set still resolves.

**Not imported.** The archive's rendered HTML dossier, which is a presentation of the same
material rather than a source, and the accepted design document itself, whose content is
distributed across the curated pages rather than duplicated.

**Precedence established.** Where a curated page and a history page disagree, the curated
page wins — history documents were not edited to stay consistent with later decisions. The
twelve [amendments](design/amendments.md) override the accepted candidate design wherever
they differ.

## 2026-08-21 — M0 maintenance corrections

Adversarial review of the integrated bundle corrected these facts (code and the
critique source win over curated prose):

- The critique's gap count is **30 enumerated gaps across sections A–H**, not 22;
  the wrong count appeared on seven pages and traced to trusting a summary of the
  review rather than the review itself. All seven corrected.
- The naming claim over `history/` was over-broad: the post-decision addendum was
  written *after* the rename and mixes both names. `history/index.md`, `index.md`,
  and this log now say so.
- `platform-contract.md` now states its CI enforcement is designed, not yet built.
- `install.sh` gained the documented `--restore` / `--rollback --image-tag` forms,
  resolving the contradiction with `operations/disaster-recovery.md`,
  `operations/bootstrap.md`, and `design/amendments.md` in code's favor by making
  the docs true.
- Effort figures: ADR 0001's comparison row now carries its units caveat;
  `delivery.md` explains the ~22 vs ~23 FTE-week split.

## 2026-08-21 — M1a implementation landed

The secrets envelope and the store/queue core now exist in code; pages describing
them are design-accurate but the implementation is authoritative:

- The recovery card's derivation block is executable text enforced by test
  (`internal/secrets/kit_test.go` runs the card's age + openssl lines verbatim and
  the derived password opens a real restic repository). The card uses `od`/`tr`
  instead of `xxd` for bare-host portability.
- The HKDF domain-separation salt is live: `exocomp/v1`
  (see [ADR 0003](adr/0003-secrets-envelope.md); the archive's `control/v1` was
  never used by any artifact).
- The job state machine, append-only `job_event` (trigger-enforced), per-key
  serialization, idempotency dedup, and the interrupted-job boot sweep are
  implemented and race-tested as designed in
  [architecture/execution.md](architecture/execution.md).

## 2026-08-21 — M1b implementation landed

- Ansible driver (`internal/executor/ansible.go`): env allowlist, per-job 0700
  private dir for vault-password and extra-vars files, `--vault-id` from the
  envelope-derived password; queue events pass through the redactor before
  persistence (`internal/queue/redact.go`). `exocomp playbook run` is live.
- Backup engine (`internal/backup`): three legs implemented and tested against
  real restic, a real bare git repo (single rolling orphan commit verified), and
  MinIO-under-podman for S3; `keyring.age` + `RECOVERY.md` land at every target
  root outside the restic repo; `exocomp backup run` / `exocomp restore` live.
- Correction: [operations/disaster-recovery.md](operations/disaster-recovery.md)
  staging tree updated to the implemented M1 contents (snapshot.db, keyring.age,
  MANIFEST.json); config/blobs/repo-bundle entries return with their features.
- TUI (`internal/tui`): Jobs and Backups screens, `exocomp tui`.
- `exocomp secrets add|list` seals values from stdin; playbook runs feed every
  sealed plaintext to the redactor.
- Deferred to M2, tracked: amendment-2 git-leg chunking and repo-size guard;
  S3 credentials as sealed rows (amendment 1); systemd-creds TPM sealing;
  age-plugin-yubikey recipients.

## 2026-08-21 — M1 security and DR corrections

An adversarial review of the M1 code found claims the implementation did not
keep. Corrections, code first:

- The recovery card told the operator to restore "using the read-only storage
  credential from this card". The card carries no such credential: per-target
  read-only keys are [amendment 1](design/amendments.md), scheduled for M2. The
  card now says the operator supplies the uri and its credential.
  [disaster-recovery.md](operations/disaster-recovery.md) still describes the
  amendment-1 end state, which is design, not current behaviour.
- `exocomp doctor --deep` does not exist. The card says `exocomp doctor`; the
  operations pages still name the designed `--deep` form.
- `install.sh --restore` without an exocomp binary printed "would restore" and
  exited 0 — a recovery entrypoint reporting success having done nothing. It now
  runs the card's chain in POSIX shell (age, openssl, restic, coreutils),
  verifies the restored files against MANIFEST.json, and fails non-zero at every
  step. `packaging/restore_test.go` drives it against a real backup.
  keyring.age is read from a local target root; an s3 or git target needs the
  operator to fetch keyring.age and pass `--keyring`.
- `backup canary` is gone; `secrets add` supersedes it (it was listed above as
  deferred to M2).
  age-plugin-yubikey recipients; `backup canary` removal in favor of `secrets add`.

## 2026-08-21 — M1 gate corrections

Adversarial review of the M1 gate found curated pages describing a not-yet-built
executor and CLI surface as though they already shipped (code wins, per
[AGENTS.md](../AGENTS.md)):

- **Executor substitution.** `requirements.md`, `overview.md` (the diagram node and
  a new explicit carve-out, below), `delivery.md`, and both affected
  `sbom/bill-of-materials.md` rows named `ansible-runner` with per-task JSON
  events as the run engine. M1's actual executor
  (`internal/executor/ansible.go`) is a direct `ansible-playbook` subprocess with
  stdout scanned line by line into events and exit-code-only correctness;
  `ansible-runner` is unused in M1 and re-evaluated for M2.
- **`operations/disaster-recovery.md` legs and failure-mode tables.** The git
  leg's 90 MB chunking and 4 GB size guard (amendment 2) are M2, not shipped —
  `internal/backup/git.go` pushes the archive whole with no size guard. Every
  command in the failure-mode table now matches the shipped CLI: `exocomp
  restore` takes `--from`/`--dest`/`--identity` and restores latest only (no
  `--snapshot` selector, no in-place restore); `exocomp recovery-kit` only
  re-renders stored kit structure and cannot mint or revoke a recovery identity;
  `exocompd serve` itself is not implemented in M1, so startup integrity
  gating, per-target alerting, the weekly recipient nag, and the bootstrap
  wizard are all M2.
- **`architecture/overview.md`'s "one queue, no side doors" invariant.**
  `exocomp backup run` and `exocomp restore` are direct CLI invocations in M1,
  outside the queue — there is no scheduler yet to enqueue them against, and
  the queue's guardrails do not apply to them. The page now states this
  explicitly as an M1 carve-out rather than leaving the exception silent; they
  become queued jobs once the M2 scheduler exists.
- **`api/` lands in M2, not M1.** `AGENTS.md`'s layout table and
  `internal/api/doc.go` both said the OpenAPI spec and typed client land in M1;
  neither exists yet. Corrected to M2.

## 2026-08-21 — operations pages aligned with the shipped surface

`doctor --deep`, `jobs list`, and the card-carried storage credential were still
described as current in three spots the M1 corrections missed; each now names the
shipped command or marks the capability M2.

## 2026-08-21 — MinIO assessment added

[sbom/minio-assessment.md](sbom/minio-assessment.md): AGPL-as-test-fixture imposes no
obligation; the real finding is CE's archival (2026-04). Fixture swap to SeaweedFS
backlogged for M2b/M3; BOM's operator-store row no longer suggests MinIO for new
deployments.

## 2026-08-21 — persona review synthesized

[design/user-feedback.md](design/user-feedback.md): four adversarial persona reviews
(veteran homelabber, SRE-at-home, time-poor generalist, security engineer). Five
convergent findings folded into M3 scope; ranked v1.1 backlog established;
[ADR 0007](adr/0007-audit-chain-deferral.md) records the previously silent audit-chain
deferral.

## 2026-08-21 — daemon redaction resolves per job

The m2 gate caught the daemon snapshotting sealed secrets at startup: a secret
sealed after `exocomp serve` started was not redacted until restart. The queue
now takes a redactor provider resolved at job claim time
([operations/serve.md](operations/serve.md), "Redaction freshness").

## 2026-08-22 — approval surface contract adopted

The design exploration settled the phone approval flow's semantics; the seven-point
contract is recorded in [design/ui-brief.md](design/ui-brief.md) and binds the M3
web/approvals implementation.

## 2026-08-22 — ADR 0008: step ledger over DBOS

Durable-execution assessment recorded: DBOS Go+SQLite is the one invariant-
compatible engine but is deferred (young SQLite path, single-maintainer, second
runtime); M3 builds rotation/upgrade on a step-ledger extension of the job store.

## 2026-08-22 — ADR 0008 fact corrected

SQLite is DBOS's default system database, not merely optional (verified against
their docs); their Postgres-for-production advice is about multi-server
distribution, which this project's invariants exclude. The deferral rationale
(single-maintainer, second execution runtime) is unchanged.

## 2026-08-22 — ADR 0009: LAN access tokens

Operator decision: LAN exposure is secured by bearer tokens minted and managed
via local auth only, with default self-signed TLS and fingerprint pinning
([ADR 0009](adr/0009-lan-access-tokens.md)). Binds the M3 web lane and adds four
v1.0 gate assertions.

## 2026-08-22 — design handoff v0.1 imported

Tokens landed verbatim at `internal/web/tokens.css` (annotated contrast, split
accent, signal system, type ramp). Artboard captures + the canvas PDF live under
[design/reference](design/reference/). Two reconciliations recorded for M3:

- **The printed card's derivation block must render from code, not the mock.**
  The design shows a per-install random salt (section 3) and `info =
  "exocomp/v1/" + purpose`; the shipped envelope uses the constant salt
  `exocomp/v1` with `info = purpose:id` (ADR 0003, CI-executed card test). Code
  wins; the card layout is adopted, its derivation text regenerates from the
  implementation.
- **The mock's `exo` command shorthand does not exist**: ADR 0002 forbids an
  `exo` alias (exo-lang collision). The kit/wizard commands render as `exocomp`;
  the new surfaces the design assumes — `kit derive [--list]`, `kit verify`,
  `identity migrate` — are adopted into the M3 CLI scope.

## 2026-08-22 — TUI design settled

The TUI canvas answered its brief in full; adopted decisions and the one new
engine requirement (notification-mute) recorded in
[design/tui-brief.md](design/tui-brief.md).

## 2026-08-22 — design handoff v0.2 (TUI) imported

TUI round as text under [design/reference/tui](design/reference/tui/): frames,
keymap, colors, copy, states — the authoritative artifacts (PNGs for looking
only). The twelve frames are copied into `internal/tui/testdata/frames/` as
golden fixtures the M3 TUI View() tests assert against; all pass their declared
width invariant (80/120/60). Canvas PDF v0.2 committed. Both standing
corrections (`exocomp` not `exo`, derivation-from-code) honored by the handoff.
Tokens refreshed from source.
## 2026-08-22 — `local-sandboxed` shipped, and one ADR 0005 claim corrected

The nono tier is the default on Linux: per-kind profiles, a startup probe that
asserts rather than assumes, and a loud degradation to plain `local` on any host
that cannot enforce it. Three corrections the executed probe forced, code
winning over the design as usual:

- **A pathname `AF_UNIX` connect is not mediated by any Landlock ABI.** Landlock
  hooks path opening; `connect(2)` on a unix socket opens nothing. Abstract
  sockets are scoped and refused; pathname ones are not. ADR 0005's socket
  denial now holds by placement — no exocomp unix socket exists for a job to
  name, the daemon's own surface is loopback TCP, no podman socket is mounted —
  and `doctor` prints the gap instead of implying a denial. The ADR's insistence
  on asserting is what found this.
- **Egress is filtered by port, not by destination.** Landlock's network rules
  match the TCP port alone, and nono's per-domain filtering routes through an
  HTTPS-terminating proxy — unusable for SSH, wrong for provider signature
  verification. Ansible gets TCP 22, tofu gets 443 plus the loopback backend
  port. Which *hosts* a job reaches stays a network-layer question.
- **`<data dir>/run` must stay under 57 bytes.** The tier redirects `TMPDIR`
  beneath it and the tools there coordinate over unix sockets placed inside it;
  `AF_UNIX` addresses cap at 108. The probe measures it and degrades loudly
  rather than letting each job discover it as an unexplained timeout.

## 2026-08-22 — image toolchain baked in (ADR 0006)

`packaging/Containerfile` builds the real Python layer: uv-managed CPython 3.14.0
plus `ansible-core==2.21.3` with `ansible-runner==2.4.3`, resolved through the
committed `toolchain-constraints.txt`. tofu, restic, age, sops, and nono are
fetched by sha256 from `tool-digests.txt` for both architectures; git alone comes
from Fedora's signed repository, because upstream publishes no binary release to
pin, and its resolved version is recorded in the image's `MANIFEST.json`.
`make budgets` measures the two numbers the bill of materials commits to and
exits non-zero above either ceiling: 592 MB against 800, 17 MB idle RSS against
80. The uv escape hatch stays opt-in — the image's `UV_TOOL_DIR` is baked, not
the volume, because a fresh restore has the image before it has the volume.

## 2026-08-22 — Ansible & OpenTofu expert reviews

Two practitioner reviews ([design/expert-reviews.md](design/expert-reviews.md))
converge: the execution cores are correct but the credential plane binding sealed
secrets to run env (provider env / plugin env / TF_VAR_) is missing, and the
sandbox allowlist starves dynamic inventory and providers. v1 must-fixes fold into
the M3b source-model scope; both lists recorded.

## 2026-08-22 — M3b glue: run-path credentials and v1 surfaces

The credential-plane binding is wired end to end: a run carries its `run_secret`
binding and inventory names on the job row and resolves them to named env at
execution — sealed values decrypted through the envelope for that run alone,
absent from the row and scrubbed from events. This is what lets a dynamic
inventory or a provider authenticate under the sandbox (stories 1/3/4/5).
[operations/sources.md](operations/sources.md).

Surfaces and deferred must-fixes landed with it: `workspace add --state-only`
emits the mode-A backend block + bearer; `workspace prune` applies a keep-last-N
+ last-M-days state policy; the web dashboard shows MUTED (jobs still run)
distinct from PAUSED; `serve --web-addr` runs the token-gated TLS LAN surface
beside the loopback state backend ([ADR 0009](adr/0009-lan-access-tokens.md));
the offline tofu path regenerates the lock from the fs-mirror with `h1:` hashes;
`host onboard` records `known_hosts` into the backed-up store, ensures the fleet
key, and seeds the onboarding runbook; and setup step 4 persists the operator
name and ntfy topic and sends (or clearly declines) the test push. Operator
pages: [sources](operations/sources.md), [LAN access](operations/lan-access.md),
[setup and notifications](operations/notifications.md).

## 2026-08-23 — GoAlert transport and job-failure paging

Two alerting features share the notify layer and the serve wiring. A GoAlert
Generic API transport joins ntfy: both are loaded from sealed secrets, nil when
unconfigured, and fan out together, so an operator seals one, the other, or
neither and still gets a working daemon. A firing notification opens an incident
and a resolved one closes the matching dedup; the integration key stays in the
envelope and never reaches a log or an error string, even on a transport
failure.

The reactive sweep gained a second half beside drift: every job that ends in a
terminal failure pages the operator once, at high priority, attributed by
runbook and workspace so a scoped mute holds it and carrying the run's last
error. A failed scheduled converge or backup no longer runs silent. Idempotency
reuses the drift sweep's event-table checkpoint unchanged — a `job-outcome`
event whose dedup key names the job — so the same job never pages twice across
repeated sweeps or a restart. [architecture/reactive.md](architecture/reactive.md),
[operations/notifications.md](operations/notifications.md).

## 2026-08-23 — Remote writes and live job follow

The CLI's client mode gained the other half of the seam: with a remote selected,
`playbook run`, `workspace plan|apply` and `approval decide` POST to that
daemon's `/api/v1` write routes instead of touching a local data dir, and
`jobs follow` watches a job's events over the SSE stream. A remote run follows
the job to its end and exits with the job's own code — the same contract a local
run gives a script — with `--no-follow` for the enqueue-and-walk-away shape.
`--output json` re-emits the daemon's own `job.enqueued` / `approval.pending` /
`approval.decision` envelope byte for byte, and `--idempotency-key` travels as
the header, so a retry dedups to one job.

Following is not a naive tail: the daemon's stream is authoritative only from the
moment it attaches, so every attach is paired with a read of the append-only
event log from the last sequence, deduped by sequence, and a dropped connection
is re-established once from that cursor. Inputs that name a file on the calling
machine — a playbook path with no `--source`, an inventory path, `-e @file`, a
plan by path — are refused rather than sent to be resolved on the other box.
`workspace plan`'s local exit-2-on-changes has no remote equivalent: it is read
from the run's driver output, which the event projection does not carry.
[operations/remote-cli.md](operations/remote-cli.md).

## 2026-08-23 — Wire the error taxonomy to the process exit code

`cmd/exocomp/main.go` exited `1` on any command error, so the taxonomy exit codes
(`internal/wire`'s `not_found`/`forbidden`/`conflict`/`kdf_busy` → 3/4/5/7) were
documented but unreachable — an agent branching on the process exit code alone
only ever saw 0, 1, or, on the handful of commands that call `os.Exit` for their
own run outcome, 2. A new `exitCodeFor` (`cmd/exocomp/exitcode.go`) is the one
mapping site: a local error resolves through `wire.ExitFor`, a remote
`client.APIError` through its own `ExitCode()` — the same table either way, so a
`not_found` exits 3 whether it was decided locally or by a remote daemon. The
per-command `os.Exit(code)` calls for a run's own outcome are untouched; they
bypass `Execute()`'s returned error by construction and were never in scope.
[operations/machine-output.md](operations/machine-output.md).

## 2026-08-23 — A governed run type for everything else

Work that is neither ansible nor tofu — a `kubectl rollout`, a scoped token refresh, a
vendor deploy CLI — reached the fleet through an `ansible shell` wrapper, which is a way of
avoiding this control plane's guarantees rather than using them: the queue saw an ansible
job, the argv on the row was a shell string, and nothing decided which programs could run.
Executor kind `cli` runs one such binary under the machinery every other kind gets — the one
queue, the sandbox, a per-job private directory, run-secret injection, redaction, the
approval gate, and exit-code-is-truth.

What makes it governable rather than a general-purpose escape hatch is the allow-list:
`exocomp cli-tool add` registers which binaries may run, with the fixed argv prefix, the
anchored argument pattern, and the egress posture each is registered for, and `run-cli`
refuses an unregistered name *before* a job row exists. The registry is a table, beside
sources and runners, so the boundary deciding what may execute is restored by the same
backup as the rest of the control plane. Registering takes the confirm contract because it
widens that boundary; removing does not, because it narrows it.

The argv is a list end to end — assembled as a slice, stored as a slice, `execve`'d as a
slice — so a metacharacter in an argument is one literal argument and not an injection
surface; the mutation that routes it through `sh -c` fails the test that pins this. The
sandbox profile is default-deny with **no** kind-wide egress: `ansible` and `tofu` each
speak one protocol, but the `cli` kind is every allow-listed binary at once, so a port is
granted per registration to that run alone.

Deferred: the daemon's kind router lives in a file another lane owns, so `run-cli` refuses
while `exocomp serve` is up rather than writing a row that would die on a worker. The
health-gate composition and the web/MCP surfaces for the kind are not built.
[operations/cli-run.md](operations/cli-run.md).

## 2026-08-23 — Blue team on the agent-facing surface

The second adversarial pass went at the three programmatic faces — the write
`/api/v1`, `exocomp login` + `--remote`, and the stdio MCP server — and every
exploited finding is closed, each one reproduced as a failing test first
([security/threat-model.md](security/threat-model.md), AI-1..AI-13).

Four operator-visible behaviours changed. Approvals over `/api/v1` now take an
`approve`-scope token: the scope that enqueues a gated apply may not settle it,
and no principal may decide an approval for a job its own token asked for. On a
workspace with enabled plan policies, plan-then-apply-by-id is the only path — a
fresh apply has no plan for a policy to read, and a plan carrying no recorded
verdict is unconsumable rather than unpoliced, which also closes the window
between the seal and the verdict. Accepting a *changed* certificate fingerprint
takes the new login-only `--replace-pin`; `--assume-yes` pins a first fingerprint
and never replaces one, because it is the flag every script already passes. And
the MCP stdio face no longer offers `approve_job`/`reject_job` at all: a
transport that carries no credential must not carry the release for the gate its
own `workspace_apply` raises.

The rest is boundary hardening: `/api/v1` writes require a bearer (no ambient
cookie), idempotency keys are namespaced per token and route, a saved plan is
bound to the workspace that produced it, a `--source` playbook is confined to its
checkout, the health probe is cached instead of forking per request, failed
bearer auth backs off with a constant-cost failure path, an open SSE stream ends
when its token is revoked, and MCP writes are attributed and answerable.

## 2026-08-23 — ADR-0014 ratified

Owner accepted the reconciled one-MCP-HTTP-surface framing; status flipped to Accepted.
