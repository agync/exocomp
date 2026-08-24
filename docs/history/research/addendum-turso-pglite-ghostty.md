# Research addendum — Turso, PGlite, Ghostty

Date: 2026-08-21 · Status: reviewed post-decision, against the accepted thin-core design
(`../design/DESIGN.md`). Verdicts feed DESIGN.md §"deliberately not adopted".

Evaluation frame — the five pillars any storage/UI component must survive:
single container · no external services · `CGO_ENABLED=0` static builds (amd64+arm64) ·
nothing stored in a bespoke format · disaster recovery independent of any vendor.

---

## Turso (libSQL + Turso Database)

**Two distinct things, both live in 2026.** (1) **libSQL** — Turso's 2023 SQLite fork,
MIT, mature and maintained; its self-hostable server `sqld` is **archived**. The Go
binding `go-libsql` (embedded-replica sync) **requires CGO**, ships prebuilt libs for
linux/darwin only, and does not cross-compile. (2) **Turso Database** (formerly Limbo) —
ground-up Rust rewrite of SQLite, MIT, **beta, pre-1.0** (~v0.7.x as of mid-2026). The
maintainers say it plainly (Pekka Enberg): "currently in beta… for mission-critical
workloads that need a battle-tested foundation today, libSQL is the right choice."
Sources: https://turso.tech/what-is-turso · https://x.com/penberg/status/2032373944007688226 ·
https://pkg.go.dev/turso.tech/database/tursogo · https://news.ycombinator.com/item?id=46810950

- **File format**: Turso Database uses the real SQLite on-disk format — `sqlite3`-openable,
  so "nothing bespoke" holds.
- **New fact worth tracking**: `tursogo` (github.com/tursodatabase/turso-go) is a
  **non-CGO** Go driver via purego/dlopen — it passes the `CGO_ENABLED=0` check but is
  *not* fully static: it `go:embed`s a native `.so` and **extracts + dlopens it at
  runtime**, requiring a writable exec-permitted path and a dynamic loader in the
  container. Pre-1.0.
- **Sync/replication** (embedded replicas, "Turso Sync", beta Oct 2025) centers on
  **Turso Cloud** URLs and auth tokens; no supported point-at-your-own-S3 mode; `sqld`
  archived. Self-hosting the protocol is community DIY.
  https://turso.tech/blog/introducing-databases-anywhere-with-turso-sync

| Pillar | libSQL (`go-libsql`) | Turso Database (`tursogo`) |
|---|---|---|
| CGO-free static build | ✗ CGO required, no cross-compile | ~ passes CGO check, but runtime-extracted `.so` |
| No external services | OK standalone; sync assumes Turso Cloud | OK standalone; sync assumes Turso Cloud |
| Plain SQLite file | ✓ | ✓ |
| Vendor-free DR | ✓ (without sync) | ✓ (without sync); Litestream still applies |
| Production ready | ✓ | ✗ explicit beta, pre-1.0 |

**What it would buy controld**: essentially nothing — the differentiators (embedded
replicas, cloud sync, MVCC/async IO) serve multi-node/edge fan-out, explicitly out of
scope. **What it would cost**: a beta engine under the source of truth, a runtime-dlopen
wrinkle in the static-build story, and sync gravity toward a vendor exactly where the
design mandates independence.

**Verdict: discard (defer-watch on Turso Database).** modernc.org/sqlite already
satisfies every pillar today; revisit only if Turso Database reaches 1.0 *and* a
self-hosted sync target materializes — and even then it competes with Litestream, not
with SQLite.

---

## PGlite

**Status.** `@electric-sql/pglite` 0.5.x; v0.4 (2026-03-25) split initdb out of the main
WASM binary, added PostGIS and community connection multiplexing. Huge adoption
(~13M weekly npm downloads) but pre-1.0. Dual Apache-2.0/PostgreSQL license.
https://electric.ax/blog/2026/03/25/announcing-pglite-v04 · https://pglite.dev/docs/about

- **Runtime**: an Emscripten build of Postgres targeting **JS hosts** (browser/Node/
  Bun/Deno), not a standalone WASI module. The one Go/wazero embedding attempt
  (`tw1nk/go-pglite`) self-describes as barely-a-REPL. **There is no supported CGO-free
  way to embed PGlite in a Go binary.**
- **Single connection**: Emscripten forbids `fork()`, so Postgres runs single-user mode;
  the v0.4 multiplexer fakes concurrency and the docs say "not all use cases are
  guaranteed to work."
- **pglite-socket/server**: exposes the wire protocol to external clients — positioned
  for dev/testing, no SSL, inherits the single-connection caveats.
- **Persistence**: a real Postgres data directory — not bespoke, but version-locked
  Postgres internals; DR means "matching binary build", not "copy one file".

**Could it un-reject Windmill / OTF / Terrakube? No.** Those are backend services
expecting normal multi-connection Postgres (pools, concurrent transactions,
LISTEN/NOTIFY). And running PGlite as a socket sidecar reintroduces exactly the
separate-stateful-DB-process that "no external DB" exists to eliminate — it relocates
the weight, it doesn't remove it.

**What it genuinely enables (elsewhere)**: local-first JS/TS apps, browser demos, edge
functions, and testing Postgres-dependent code without Docker. The opposite of
controld's problem, which embedded SQLite already solves natively.

**Verdict: discard.** No supported CGO-free Go embedding path, and its escape hatch
(a JS-side socket server) is the external stateful service the design forbids.

---

## Ghostty / libghostty (wasm)

**Status.** Ghostty the app is mature (1.2.0 2025-09, 1.3.1 2026-03). **libghostty** was
announced 2025-09-22 as a family of embeddable libraries, starting with
`libghostty-vt` — parser/terminal-state only, no rendering/GPU/windowing; Zig API alpha,
no stability guarantees. By 2026-02 `ghostty-vt.wasm` ships prebuilt and benchmarks
faster than xterm.js; `coder/ghostty-web` (MIT, ~400 KB) wraps it with an
xterm.js-compatible API (one downstream user: Coder). The full app-embedding C API is
macOS/iOS/Linux-only and "not yet stabilized for general-purpose embedding"; maintainers
called an earlier full-app wasm branch a dead end.
https://mitchellh.com/writing/libghostty-is-coming · https://libghostty.tip.ghostty.org/ ·
https://github.com/coder/ghostty-web · https://ghostty.org/docs/install/release-notes

Three conceivable fits, assessed:

1. **Operator's terminal for `ssh control.home`** — real, works today, and requires
   nothing from us. Ghostty is a *beneficiary* of the TUI-over-SSH design, not a
   component of it.
2. **ghostty-wasm as in-browser terminal in the web UI** — technically the strongest
   fit (`ghostty-web` is a near drop-in xterm.js replacement with a more correct
   parser), but the design deliberately chose *not* to do terminal-in-browser: the web
   tier exists for the phone-tap approval flow, which wants plain HTML buttons. A
   better terminal library doesn't reverse that call; it only names the library we'd
   pick **if** we ever added an optional in-browser TUI view. Recorded as the seam.
3. **libghostty as the desktop shell instead of Wails** — no fit today: no Windows, no
   webview-hosting story, unstable API, one real consumer (Ghostty's own mac app).

**Verdict: defer-watch.** Pre-1.0 everywhere it could touch this project; changes no
decision today. Bookmark `ghostty-web` as the chosen library for a future opt-in
web-TUI panel, should that ever be wanted.

---

## Net effect on DESIGN.md

No accepted-design change. Three entries added to "deliberately not adopted":
Turso (beta engine / CGO / vendor-gravity sync), PGlite (no Go embedding; the sidecar
form is the forbidden external DB), Ghostty (beneficiary, not component; `ghostty-web`
noted as the seam for a hypothetical in-browser TUI).

---

## nono (nono.sh) — added 2026-08-21

**Status.** Open-source (Apache-2.0) Rust CLI + library from nolabs-ai (Luke Hinds,
Sigstore founder); launched publicly July 2026, ~3.1k stars, 80+ contributors.
Kernel-enforced capability sandbox for processes: **default-deny filesystem via
Landlock (Linux) / Seatbelt (macOS)**, network filtering via seccomp
`SECCOMP_RET_USER_NOTIF` on `openat/bind/connect`, declarative profiles, capabilities
fixed at start and unwidenable from inside. Unprivileged self-restriction — no root, no
daemon, works under rootless podman. One security event so far: **CVE-2026-47128**
(sandbox escape via the per-user systemd D-Bus unix socket, fixed in 0.55.0) — evidence
of both active maintenance and the inherent escape class of local-socket access.
Sources: https://github.com/nolabs-ai/nono · https://nono.sh/cli ·
https://www.helpnetsecurity.com/2026/07/27/nono-open-source-ai-agent-sandboxing/ ·
https://advisories.gitlab.com/cargo/nono-cli/CVE-2026-47128/

**Fit.** Unlike Turso/PGlite/Ghostty, this lands squarely on an open problem the design
already named: §7.2's trade was `local` (arbitrary playbook code shares a namespace
with `exocomp.db`, the host key, and the API socket) vs `container` (privilege
separation, but controld holds the podman socket and EE images strain DR). nono is the
**missing middle tier**: kernel default-deny around each job subprocess, no container
runtime, no socket paradox, no registry on the DR path.

- Job profile shape: allow the job tmpfs, repo checkout, artifact dir, tool paths, and
  provider/collection caches; deny everything else — explicitly including
  `exocomp.db`, `host.key`, other jobs' tmpfs, **the exocomp API socket, and any podman
  socket**. (CVE-2026-47128's lesson generalized: the escape class is "sandboxed
  process reaches a powerful local unix socket," and exocomp ships exactly such a
  socket.)
- Network: ansible needs SSH to hosts; tofu needs provider endpoints + the
  127.0.0.1 state backend — expressible as per-runbook-kind profiles.
- Thin-core aligned: shelling out to a pinned `nono run` is exactly the "never
  reimplement a tool that exists" pattern; fallback if the dependency sours is
  in-process `go-landlock` self-restriction (fs-only, no new binary).
- Caveats: young project; Landlock network rules want a recent kernel (ABI v4+,
  ≥6.7) — `exocomp doctor` must probe the ABI and degrade loudly; require ≥0.55.0;
  nono's agent-oriented features (AGENTS.md signing, snapshot rollback, credential
  injection) are unused here.

**Verdict: adopt** as a `local-sandboxed` executor tier — default-on for Linux job
execution where the kernel supports it — recorded as DESIGN.md §7.3. It captures most
of the `container` driver's privilege-separation prize at a fraction of its cost, and
demotes that driver to "per-project EE versioning and hard resource caps only."
