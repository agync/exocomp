# exocomp — UI design brief

For a designer with no prior context. Direction: clean, minimal, modern, functional,
accessible. This brief covers the web UI (priority 2 surface) and the phone approval
flow; the TUI exists and sets the tone — the web UI is its calmer sibling, not a
marketing site.

## What exocomp is

A homelab control plane: one small daemon that runs Ansible playbooks and OpenTofu
plans/applies, holds secrets in an encrypted envelope, backs itself up to three places,
and (soon) reacts to monitoring alerts with approval-gated runbooks. Single operator, a
handful of machines, self-hosted. The product's core promises: nothing runs outside one
audited job queue, secrets never appear anywhere, and the whole system restores from
one printed recovery card.

## The user

One person. Expert, terminal-native, runs this for their own infrastructure. Two
contexts, two postures:

- **At a desk** — SSH/TUI or browser. Wants density, keyboard flow, fast answers.
- **On a phone** — arrives via a push notification. Wants one decision with enough
  context to make it, one thumb, under ten seconds.

No teams, no roles, no onboarding funnel, no persuasion. The UI never sells; it reports.

## Design principles

1. **Status before detail.** The first screen answers "is everything OK?" without
   scrolling: last jobs, backup freshness, drift, pending approvals.
2. **Failure is louder than success.** Healthy states are quiet (dim, small); failures,
   staleness, and waiting-on-human states carry the visual weight. An old backup is a
   warning at 24h and loud at 48h even though nothing "failed".
3. **Everything is a job.** Manual runs, scheduled runs, backups, applies — one list,
   one detail view, one event stream. Don't invent per-feature layouts.
4. **Truth over reassurance.** Exit codes, timestamps, serial numbers shown plainly;
   `tabular-nums` everywhere digits align. No progress theatre, no vanity metrics.
5. **Quiet surface, dense information.** Minimal chrome, generous type hierarchy,
   tables that respect density. Whitespace serves scanning, not aesthetics.

## Core vocabulary (the objects on screen)

| Object | What the user needs to see |
|---|---|
| **Job** | state, kind (ansible/tofu/backup/shell), trigger (manual/schedule/event), queued→started→finished times, exit code, live log tail |
| **Workspace** (tofu) | name, state serial, last plan result, last apply result, **drift marker** (last plan found changes), state version history |
| **Plan → Apply** | a saved plan is an artifact; the apply that consumes it links back — the approval view shows *the actual plan output* |
| **Schedule** | name, cron expression, kind, enabled/disabled (disabled = dimmed, not hidden), next fire, last job outcome |
| **Backup target** | name, kind (NAS / S3 / git), last run state, snapshot id, bytes added, **freshness age** with 24h/48h thresholds |
| **Secret** | name, kind, created/rotated dates — **values never render anywhere, ever** |
| **Approval** (M3) | what wants to run, why (triggering alert or plan diff), time left on the TTL, Approve / Reject |
| **Event** (M3) | every received alert with its outcome: ignored / matched / suppressed / gated / fired — "why didn't it fire?" must be answerable by scanning |
| **Recovery kit** | printable card: age secret key, recipient fingerprints, derivation recipe — print stylesheet matters more than screen styling |

## Operations, by frequency

1. **Glance** (many times daily): open, confirm quiet, close. The dashboard is the product.
2. **Approve / reject** (occasional, urgent, mostly phone): notification → context →
   decision. Big touch targets, plan diff readable on a phone, expiry visible.
3. **Run something** (weekly): pick a runbook/playbook/workspace → run/plan/apply →
   watch the live tail. Streaming log with follow toggle; state changes announced.
4. **Investigate** (when something is red): job detail, event stream, drift diff,
   which rule fired or was suppressed and why.
5. **Configure** (rare): add a schedule, backup target, workspace, secret; onboard a
   host. Forms are terse, validated, and honest about consequences.
6. **Recover** (rare, high stress): restore flow and doctor output. Assume the user is
   anxious and possibly on unfamiliar hardware: linear steps, no branching cleverness,
   every command copyable.

## States and semantic color

One accent color for interactive/running; functional colors reserved for state:
succeeded=green, failed/timed_out=red, awaiting approval/pending=amber,
running=accent, queued/disabled/skipped=dim neutral, interrupted=red-outline.
State must never be color-only — pair with a label or glyph (no emoji).

## Accessibility (hard requirements)

- WCAG AA+ contrast on every pairing, annotated in the tokens.
- Fully keyboard-operable; visible focus ring (3px, 2px offset); logical tab order.
- Light and dark themes, both first-class; `prefers-reduced-motion` collapses all motion.
- Touch targets ≥ 48px on the phone approval surface.
- Live log regions announced politely (`aria-live`), never stealing focus.
- Tables get real `<th>` scope, sortable headers announced.

## Anti-goals

No hero sections, no illustrations, no empty-state mascots, no gradients-as-decoration,
no metric cards without decisions behind them, no settings sprawl, no multi-tenant
affordances. If a screen would look at home on a SaaS landing page, it's wrong.

## Technical frame (constrains the design)

Server-rendered HTML + htmx over one JSON API; live updates via SSE (log tails, job
state). No SPA. Type system: monospace-forward is native to this product's audience —
a mono for data/labels with a quiet UI sans is acceptable; pure-mono is on-brand.
The TUI already establishes: one accent, dim-by-default, state-colored text.
