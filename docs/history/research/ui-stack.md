# UI stack research: TUI-first, web-second, desktop-third

Research date: 2026-08-21. Scope: pick a UI technology stack for a single-binary
homelab "control stack" (Ansible-Tower/AWX + Terraform-Cloud-ish functionality
for one operator and a handful of hosts), where the priority order is **TUI →
web UI → desktop app**, all three ideally sharing one backend/API.

## TL;DR

Build the core as a **Go daemon exposing a local HTTP/JSON (or gRPC) API**,
with three thin front ends layered on top of that one API:

1. **TUI**: Go + [Bubble Tea](https://github.com/charmbracelet/bubbletea) v2 +
   [Bubbles](https://github.com/charmbracelet/bubbles) +
   [Lip Gloss](https://github.com/charmbracelet/lipgloss) v2, talking to the
   local API over a Unix socket / loopback HTTP. Optionally exposed remotely
   over SSH via [Wish](https://github.com/charmbracelet/wish) for zero-install
   remote access from other homelab hosts.
2. **Web**: server-rendered Go with [templ](https://github.com/a-h/templ) +
   [htmx](https://htmx.org/), assets embedded via `go:embed`, hitting the same
   API handlers the TUI uses (thin HTML views, not a second business-logic
   layer).
3. **Desktop** (later, optional): [Wails](https://wails.io/) wraps the same
   Go binary and the same web UI in a native window when a “real app icon”
   is wanted — no separate rewrite.

This mirrors how Gitea, Miniflux, HashiCorp Vault/Nomad, and Semaphore UI all
ship as a single Go binary with the web UI embedded via `go:embed`, and it
keeps every layer talking to one API so there is only one place business
logic (playbook runs, state storage, backups, alert-triggered runbooks) can
live. Rust (ratatui) and Python (Textual) are both excellent TUI toolkits but
pull the rest of the stack (web glue, single-binary packaging, SSH serving)
in directions that fit this project less well than Go's ecosystem does — see
the per-option analysis below.

---

## Landscape table

| Name | Language / runtime | License | Footprint | Single binary? | Health (Aug 2026) |
|---|---|---|---|---|---|
| [Bubble Tea](https://github.com/charmbracelet/bubbletea) | Go | MIT | ~38k★, adds a few MB to a Go binary, no runtime deps | Yes (compiles in) | Very active; v2 shipped Feb 2026 with a new "Cursed Renderer" and synchronized-output support [[Charm v2 blog]](https://charm.land/blog/v2/) [[releases]](https://github.com/charmbracelet/bubbletea/releases). Production users cited include Azure, AWS, Cockroach Labs, Ubuntu [[Charm v2]](https://charm.land/blog/v2/) |
| [Bubbles](https://github.com/charmbracelet/bubbles) (widgets) | Go | MIT | small | Yes | Active, tracks Bubble Tea v2 [[DeepWiki]](https://deepwiki.com/charmbracelet/bubbles) |
| [Lip Gloss](https://github.com/charmbracelet/lipgloss) (styling) | Go | MIT | small | Yes | Active, v2 released alongside Bubble Tea v2 [[Charm v2]](https://charm.land/blog/v2/) |
| [Wish](https://github.com/charmbracelet/wish) (serve TUIs over SSH) | Go | MIT | small (SSH server middleware) | Yes | Active, part of Charm's maintained SSH-app stack [[pkg.go.dev]](https://pkg.go.dev/github.com/charmbracelet/wish) [[wish repo]](https://github.com/charmbracelet/wish) |
| [tview](https://github.com/rivo/tview) | Go (on tcell) | MIT | ~12k★, small | Yes | Actively maintained, push activity as recent as March 2026, 125 open issues [[ecosyste.ms]](https://awesome.ecosyste.ms/projects/github.com/rivo/tview) [[GitHub]](https://github.com/rivo/tview) |
| [Ratatui](https://ratatui.rs/) | Rust | MIT | ~20.3k★, no GC, low memory | Yes | Very active; de-facto successor to abandoned `tui-rs`, gained the 20k+ stars since 2023, "excellent" quality score [[starlog]](https://starlog.is/articles/developer-tools/ratatui-ratatui/) [[GitHub]](https://github.com/ratatui/ratatui) |
| [Textual](https://textual.textualize.io/) | Python | MIT | heavier (Python runtime + deps); no native single-binary story without PyInstaller/pyoxidizer | No (needs a Python interpreter, or a bundled one) | Framework is actively developed (v5.0 shipped Jul 2025 with markdown streaming for the "Toad" AI TUI), but **Textualize the company wound down in 2025** — maintenance is now community/solo (Will McGugan), a real bus-factor risk [[Future of Textualize]](https://textual.textualize.io/blog/2025/05/07/the-future-of-textualize/) [[Tracxn]](https://tracxn.com/d/companies/textualize/__xWa_U0-hR023fzVEf30z2Nq2jKm2CtyUcgV0TOvusu0) |
| [textual-web](https://github.com/Textualize/textual-web) | Python | MIT | adds a websocket relay layer | No | Public beta status; "sessions not yet implemented," known rendering glitches with heavy color use; self-hostable but the polished path (permanent URLs) leans on Textualize's `ansi.app` relay account system [[GitHub]](https://github.com/Textualize/textual-web) |
| Go web UI via `go:embed` + [templ](https://github.com/a-h/templ) + [htmx](https://htmx.org/) | Go + HTML/JS | templ: MIT; htmx: 0BSD | tiny (no Node build for htmx; templ compiles to Go) | Yes | Both actively maintained; pattern proven in production Go apps [[Guillembonet example]](https://github.com/Guillembonet/go-templ-htmx) [[Alex Edwards]](https://www.alexedwards.net/blog/how-i-use-htmx-with-go) |
| SPA (React/Vue) embedded via `go:embed` | Go backend + JS frontend | varies (framework-dependent) | larger (Node build toolchain, bigger JS bundle) | Yes (after build step) | Proven pattern (Gitea's older UI, most dashboards), but adds a whole JS build pipeline the other two Go options avoid |
| [Wails](https://wails.io/) | Go + OS-native WebView | MIT | small (uses OS webview, no bundled Chromium) | Yes | Active; stable v2.12.0 shipped Mar 2026, targets Win/macOS/Linux [[comparison]](https://dev.to/arashgl/taurirust-vs-wailsgo-4pd6); multi-window/notifications still maturing [[HN thread]](https://news.ycombinator.com/item?id=46084442) |
| [Tauri](https://tauri.app/) v2 | Rust + OS-native WebView, JS frontend | MIT / Apache-2.0 | sub-600KB core | Yes | Active, "default for desktop in 2026" per comparison pieces; steeper learning curve if the backend isn't already Rust [[comparison]](https://www.digitalapplied.com/blog/desktop-apps-web-stack-tauri-electron-deno-wails-2026) |
| [ttyd](https://github.com/tsl0922/ttyd) / [GoTTY](https://github.com/sorenisanerd/gotty) (terminal-over-websocket) | C / Go | MIT | tiny | Yes (separate binary) | Mature, low-maintenance utilities; screen-scrape any TUI into a browser via xterm.js, no app awareness [[ttyd docs]](https://tsl0922.github.io/ttyd/) [[gotty]](https://github.com/sorenisanerd/gotty) |

Reference points for "single Go binary with embedded web UI" done well at
production scale: **Gitea** (bindata/`go:embed` bundling, layered
`cmd → routers → services → models`) [[DeepWiki]](https://deepwiki.com/go-gitea/gitea/1.1-architecture-overview),
**Miniflux** (`go:embed` for all static assets) [[repo]](https://github.com/miniflux/v2),
and **HashiCorp Vault/Nomad**, whose Ember.js UI is compiled to static assets
and embedded into the binary via `go:embed` for production builds
[[DeepWiki: Vault UI]](https://deepwiki.com/hashicorp/vault/7-user-interface).
**Semaphore UI** — an open-source, MIT-licensed, single-Go-binary Ansible +
Terraform/OpenTofu web runner with scheduling, RBAC, and Git integration — is
close to a sibling project for the automation half of this stack (web-only,
no TUI, no SSH-serving, but proof that "AWX + Terraform Cloud, but a single
Go binary, ~50MB RAM idle" is a viable shape)
[[Semaphore vs AWX]](https://semaphoreui.com/blog/awx-vs-semaphore).

---

## Per-option analysis

### Go TUI: Bubble Tea + Bubbles + Lip Gloss (Charm ecosystem)

The Elm-architecture (`Model`/`Update`/`View`) framework is the most widely
adopted Go TUI stack, with Bubbles supplying ready widgets (tables, lists,
text inputs, spinners, file pickers, viewport/pager) and Lip Gloss handling
CSS-like styling and layout. Bubble Tea v2 (Feb 2026) added a new renderer
("Cursed Renderer," ncurses-derived) and terminal synchronized-output support
for flicker-free redraws [[Charm v2 blog]](https://charm.land/blog/v2/).
Adoption is broad — cited production users include Azure, AWS, Cockroach
Labs, Ubuntu, and NVIDIA — and the license (MIT) is fully permissive.

Because it's plain Go, it compiles straight into the same binary as the
control-plane daemon: no interpreter, no separate runtime, no FFI boundary.
This is the single biggest reason it fits a "single Go binary" project better
than Textual (Python) or Ratatui (Rust, if the rest of the stack is Go).

**Alternative widget library — tview**: simpler, more traditional
"retained-mode" widget tree (forms, tables, tree views) built on `tcell`.
Actively maintained (push activity as recent as March 2026) with 12k★ but a
noticeably smaller ecosystem than Bubble Tea's. Good if the app is more
form/CRUD-shaped (think: editing inventory, scheduling jobs) than
dashboard/animation-shaped; Bubble Tea/Bubbles is more idiomatic for
dashboards, live job-log tailing, and anything needing custom rendering.

### Rust TUI: Ratatui

Ratatui (successor to the abandoned `tui-rs`, revived 2023) is the Rust TUI
of choice: immediate-mode rendering, MIT-licensed, ~20.3k★, and genuinely
excellent performance/memory characteristics (30-40% less memory, ~15% lower
CPU than comparable stacks in one benchmark) [[starlog]](https://starlog.is/articles/developer-tools/ratatui-ratatui/).
Health is excellent — very active maintenance, large `awesome-ratatui`
ecosystem [[awesome-ratatui]](https://github.com/ratatui/awesome-ratatui).

For *this* project the problem isn't Ratatui's quality, it's ecosystem fit:
picking Rust for the TUI means either (a) writing the whole control-plane
daemon in Rust — a bigger lift for the parts of this project that mostly
want to shell out to `ansible-playbook`/`terraform`/`sops`/`age` and glue
config together, where Go's simplicity and stdlib HTTP/exec ergonomics win —
or (b) a second-language split (Rust TUI, Go daemon) with an API boundary
between them, which adds real complexity for a homelab-scale, single-operator
project. Ratatui is the right call if the whole stack were being built in
Rust; it's not worth introducing a second language just for the TUI layer.

### Python: Textual (+ textual-web)

Textual is the most "batteries-included" TUI framework of the three,
CSS-like styling, reactive widgets, and a genuinely good browser story via
**textual-web**, which runs the same Python app and streams its terminal
output into the browser (over a websocket) rather than re-rendering it as
native HTML/DOM — visually it is a terminal emulator in a browser tab, not
an idiomatic web page (confirmed against the project's own docs) [[textual-web]](https://github.com/textualize/textual-web).
This is a genuinely elegant "one codebase → TUI + web" trick, and it's the
strongest argument for Textual in this whole landscape.

Two things count against it for this project:

1. **Packaging**: Python doesn't produce a true single static binary the way
   Go and Rust do. Getting a "download one file, run it" experience needs
   PyInstaller/pyoxidizer/similar, which adds build complexity and platform-
   specific quirks that Go's `go build` simply doesn't have.
2. **Project health**: **Textualize (the company) wound down in 2025** —
   Will McGugan is on sabbatical and the framework is now maintained more as
   a personal/community open-source project than a funded one, even though
   development has continued (Textual v5.0 shipped Jul 2025) [[Future of Textualize]](https://textual.textualize.io/blog/2025/05/07/the-future-of-textualize/) [[Tracxn]](https://tracxn.com/d/companies/textualize/__xWa_U0-hR023fzVEf30z2Nq2jKm2CtyUcgV0TOvusu0).
   textual-web itself is still explicitly in **beta**, with known gaps
   (no session persistence — closing the tab kills the app; color-heavy UIs
   can glitch) [[textual-web]](https://github.com/textualize/textual-web).

If the control-plane were being built in Python for other reasons, Textual +
textual-web would be an extremely strong, possibly best-in-class choice for
"one codebase, TUI and web." For a Go-shaped project it's a stack + runtime
mismatch, and the reduced maintenance bandwidth is a real risk for something
this project would depend on long-term.

### Web UI embedded in the Go binary

Two credible sub-options, both compiled into the binary via `go:embed`:

- **templ + htmx (server-rendered)**: `templ` (MIT) compiles typed Go
  functions that emit HTML; `htmx` (0BSD, formerly BSD-2) adds partial-page
  updates, polling, and websocket/SSE hooks without a JS framework or Node
  build step. This is the better fit here: no separate frontend build
  pipeline, the rendering logic lives next to the API handlers in Go, and it
  keeps the "single binary, single language" property all the way through
  the web layer. Multiple production write-ups describe this exact pattern
  for internal dashboards [[Alex Edwards]](https://www.alexedwards.net/blog/how-i-use-htmx-with-go) [[example repo]](https://github.com/Guillembonet/go-templ-htmx).
- **SPA (React/Vue/Svelte) embedded via `go:embed`**: proven at scale
  (older Gitea UI, most SaaS dashboards) but brings in a Node/npm build
  toolchain, a second package ecosystem to keep patched, and materially more
  JS shipped to the browser — overhead this project's "extremely lightweight,
  homelab scale" goal doesn't need. Worth it only if the web UI later needs
  rich client-side interactivity (drag-and-drop DAG editors, live charts)
  that htmx's server-driven model genuinely can't express well.

**Terminal-over-web utilities (ttyd/GoTTY)** are a third, much cruder option:
they screen-scrape *any* running terminal program (including the Bubble Tea
TUI itself) into a browser via xterm.js, with zero app awareness. This is a
legitimate fast path to "the TUI is also reachable from a browser" — literally
run `ttyd ./control-tui` — but it's a stopgap, not the web UI: no responsive
layout, no mobile-friendliness, no separate lighter-weight mobile views
of e.g. job status. Reasonable as a zero-effort v0, not the answer for goal 2
("web UI" as a first-class citizen).

### Desktop wrappers: Wails vs Tauri

Both let a web UI become a native-feeling desktop app by wrapping it in the
OS's native WebView (no bundled Chromium, unlike Electron).

- **Wails** (Go backend, MIT): the natural choice if the control-plane and
  web UI are already Go — it reuses the same Go code for backend logic and
  gives type-safe Go↔JS bindings "for free." Stable v2.12.0 shipped March
  2026, targeting Windows/macOS/Linux with the OS-native webview; some
  polish items (multi-window, notifications) were still maturing as of the
  comparison pieces reviewed [[comparison]](https://dev.to/arashgl/taurirust-vs-wailsgo-4pd6) [[HN]](https://news.ycombinator.com/item?id=46084442).
- **Tauri v2** (Rust backend, MIT/Apache-2.0): smaller core (sub-600KB),
  broader plugin ecosystem, generally cited as the more mature/2026-default
  desktop-from-web framework — but it wants a Rust backend to get the most
  out of it, which reintroduces the same second-language problem as Ratatui
  above [[comparison]](https://www.digitalapplied.com/blog/desktop-apps-web-stack-tauri-electron-deno-wails-2026).

Given the "TUI first, web second, desktop third" priority and an all-Go
core, **Wails is the better fit if/when a desktop app is built** — it wraps
the exact same htmx/templ web UI with no rewrite, at the cost of being
somewhat less polished than Tauri today. This is explicitly a "later" concern
per the project goals; Tauri remains a fine fallback if Wails' desktop-app
gaps (multi-window, notifications) turn out to matter and a thin Rust shim is
acceptable.

### Serving the TUI remotely: Wish (SSH)

[Wish](https://github.com/charmbracelet/wish) (MIT, Charm) is Go middleware
for building SSH servers, with first-class Bubble Tea integration: connect
via `ssh control.home`, and the SSH session's PTY is wired directly into a
`tea.Program`, window-resize messages included [[wish repo]](https://github.com/charmbracelet/wish) [[pkg.go.dev]](https://pkg.go.dev/github.com/charmbracelet/wish).
This is a very good match for the "one operator, a handful of hosts" homelab
shape: no separate web server, no browser, no port-forwarding needed to
manage the control plane from another machine on the network or a phone's
SSH client — just SSH in and the TUI is the whole interface, with SSH-key
auth as the access-control mechanism for free. Charm's own **soft-serve**
(self-hosted Git server: SSH + TUI, deliberately *no* web interface) is the
existing proof this Wish-based "single Go binary, serve the TUI over SSH"
pattern works well at this scale [[soft-serve repo]](https://github.com/charmbracelet/soft-serve).

---

## Integration notes against the project goals

- **Single-binary, homelab scale** → Go wins decisively across the whole
  stack: Bubble Tea/Bubbles/Lip Gloss for TUI, templ+htmx for web (both
  `go:embed`-able), Wails for desktop later. No interpreter, no Node
  toolchain in the hot path, no second language to keep patched. This is the
  same shape Gitea, Miniflux, Semaphore UI, and HashiCorp Vault/Nomad already
  ship in production.
- **AWX/Tower + Terraform-Cloud-in-one, homelab-sized** → the closest sibling
  in the wild is Semaphore UI (Go, single binary, MIT, ~50MB RAM idle,
  Ansible + Terraform/OpenTofu runner with scheduling/RBAC/Git integration)
  [[Semaphore vs AWX]](https://semaphoreui.com/blog/awx-vs-semaphore). It's
  web-only (no TUI, no SSH-serving, no built-in backup/DR story), so it's a
  good reference for API/data-model shape, not a drop-in replacement for
  this project's TUI-first requirement.
- **Reactive actions (Alertmanager/ntfy/PagerDuty webhooks → runbooks)** →
  belongs entirely behind the API layer, not the UI layer: webhook receivers
  post into the same job-queue endpoint the TUI's "run playbook" command and
  the web UI's "Run" button both call. None of the three UI layers need to
  know an alert triggered the run instead of a human — they just render job
  history the same way either way. This is the strongest argument for the
  "one API, three thin clients" shape: reactive automation is just a fourth,
  headless client of that same API.
- **Backups to NAS/S3/GitHub, disaster recovery** → also API/daemon-side
  concerns, orthogonal to UI choice; the TUI and web UI both just need a
  "Backups" screen that lists snapshots and triggers restore against the API.
  No UI-technology decision here changes based on the backup target.
- **Secure secrets (ansible-vault, sops/age, OpenBao-class)** → orthogonal to
  UI choice as well, but worth flagging: whichever UI framework is chosen
  must never render decrypted secret values into TUI/web state that gets
  logged or cached (e.g., avoid putting decrypted values in Bubble Tea model
  state that gets included in crash dumps, or in htmx response bodies that
  browsers cache) — a UI-layer discipline note more than a framework choice.
- **TUI reachable without a browser, over the network** → Wish/SSH covers
  this cleanly and matches "trivially easy to bootstrap/manage": `ssh
  control.home` from any device with an SSH client, no port-forwarding, no
  TLS cert to manage for the TUI path.

---

## Recommended architecture: one API, three thin clients

```
                     ┌─────────────────────────────┐
                     │   control-plane daemon (Go)  │
                     │                               │
   Alertmanager /    │  HTTP/JSON (or gRPC) API      │
   ntfy / healthchecks│  ├─ inventory, jobs, schedules│
   webhooks ─────────▶│  ├─ playbook/terraform runner │
                     │  ├─ state store + encryption   │
                     │  ├─ backup/restore engine      │
                     │  └─ job queue + event log       │
                     └───────────┬───────────────────┘
                                 │ same API, same auth
            ┌────────────────────┼────────────────────┐
            │                    │                     │
   ┌────────▼────────┐  ┌────────▼─────────┐  ┌────────▼─────────┐
   │ TUI              │  │ Web UI            │  │ Desktop (later)   │
   │ Bubble Tea +      │  │ templ + htmx,      │  │ Wails wrapping    │
   │ Bubbles + Lip Gloss│  │ go:embed'd assets  │  │ the same web UI   │
   │ compiled into the │  │ served by the same │  │ binary, no code   │
   │ same binary; also  │  │ binary's HTTP mux  │  │ duplication       │
   │ servable remotely  │  │                    │  │                   │
   │ over SSH via Wish  │  │                    │  │                   │
   └────────────────────┘  └────────────────────┘  └────────────────────┘
```

Concretely:

1. **One Go module, one binary.** The control-plane daemon exposes a local
   HTTP API (Unix socket by default; optionally loopback TCP) that owns all
   state, job execution, and secret handling. This is the only place
   business logic lives.
2. **TUI is the primary client**, built with Bubble Tea/Bubbles/Lip Gloss,
   talking to the local API. Ship it as the default `control` command with
   no flags — bootstrapping is `control` and you're in the TUI. Optionally
   register a Wish SSH server (`control serve --ssh`) so the same TUI is
   reachable as `ssh control.home` from any other host on the network,
   matching the "trivially easy to manage" and "handful of hosts" goals.
3. **Web UI is templ + htmx**, rendered by handlers that call the *same* Go
   functions the TUI's API client calls (thin HTTP handlers, not a parallel
   service layer) and served from `control serve --web`, with all HTML/CSS/JS
   embedded via `go:embed` — no separate deploy artifact, no Node build.
4. **Desktop is Wails, added last and only if wanted.** Because it's a Go
   framework wrapping the *same* embedded web UI, it's close to zero
   incremental UI work — mostly packaging (window chrome, tray icon, native
   menus) rather than a rewrite. If Wails' current gaps (multi-window,
   notifications) end up mattering, Tauri is the fallback, at the cost of a
   thin Rust shim calling back into the Go daemon (e.g., over the same local
   API) rather than a native Go backend.
5. **Reactive automation (Alertmanager/ntfy/PagerDuty webhooks) is a fourth,
   headless client** of the same API — a webhook receiver that posts a "run
   this runbook" request through the identical job-creation endpoint the TUI
   and web UI use, so job history, retries, and notifications are uniform
   regardless of trigger source.

This keeps the surface area small (one language, one API, one auth/authz
path, one place secrets ever get decrypted) while satisfying "TUI first, web
second, desktop third" as a genuine progressive layering rather than three
separate implementations to keep in sync.

---

## Sources

- Charm v2 announcement (Bubble Tea/Lip Gloss/Bubbles v2) — https://charm.land/blog/v2/
- Bubble Tea releases — https://github.com/charmbracelet/bubbletea/releases
- Bubbles (DeepWiki) — https://deepwiki.com/charmbracelet/bubbles
- Wish (SSH middleware) repo — https://github.com/charmbracelet/wish
- Wish package docs — https://pkg.go.dev/github.com/charmbracelet/wish
- Soft Serve (SSH+TUI git server, no web) — https://github.com/charmbracelet/soft-serve
- tview repo — https://github.com/rivo/tview
- tview activity (ecosyste.ms) — https://awesome.ecosyste.ms/projects/github.com/rivo/tview
- Ratatui site — https://ratatui.rs/
- Ratatui repo — https://github.com/ratatui/ratatui
- Ratatui health/performance write-up — https://starlog.is/articles/developer-tools/ratatui-ratatui/
- awesome-ratatui — https://github.com/ratatui/awesome-ratatui
- Textual site — https://textual.textualize.io/
- "The future of Textualize" (company wind-down, 2025) — https://textual.textualize.io/blog/2025/05/07/the-future-of-textualize/
- Textualize company profile — https://tracxn.com/d/companies/textualize/__xWa_U0-hR023fzVEf30z2Nq2jKm2CtyUcgV0TOvusu0
- textual-web repo — https://github.com/Textualize/textual-web
- go:embed + Vault UI (DeepWiki) — https://deepwiki.com/hashicorp/vault/7-user-interface
- Gitea architecture (DeepWiki) — https://deepwiki.com/go-gitea/gitea/1.1-architecture-overview
- Gitea from-source docs (bindata/embed) — https://docs.gitea.com/installation/install-from-source/
- Miniflux repo — https://github.com/miniflux/v2
- Semaphore UI vs AWX — https://semaphoreui.com/blog/awx-vs-semaphore
- templ repo — https://github.com/a-h/templ
- htmx license commit (0BSD) — https://github.com/bigskysoftware/htmx/commit/e16f1865a494b6281f8a44ed0db5338e718b3f07
- Go + templ + htmx example — https://github.com/Guillembonet/go-templ-htmx
- "How I use HTMX with Go" (Alex Edwards) — https://www.alexedwards.net/blog/how-i-use-htmx-with-go
- Wails site — https://wails.io/
- Wails vs Tauri comparison — https://dev.to/arashgl/taurirust-vs-wailsgo-4pd6
- Wails HN discussion — https://news.ycombinator.com/item?id=46084442
- Tauri vs Wails vs Electron 2026 comparison — https://www.digitalapplied.com/blog/desktop-apps-web-stack-tauri-electron-deno-wails-2026
- ttyd — https://tsl0922.github.io/ttyd/
- GoTTY — https://github.com/sorenisanerd/gotty
- k9s (single-binary Go TUI precedent) — https://terminaltrove.com/k9s/
- lazydocker (single-binary TUI precedent) — https://lazydocker.com/
