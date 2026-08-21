# Reactive / Event-Driven Automation for a Homelab Control Stack

Research note — surveys tools that can turn an alert (Alertmanager, Uptime Kuma,
healthchecks.io, a webhook, a cron heartbeat) into a runbook execution, with an
optional human approval gate, at homelab scale (one operator, a handful of hosts).
Evaluated against: footprint, single-binary/single-container friendliness,
license, and 2025–2026 maintenance health.

Sources are cited inline; all links were checked during this research pass
(August 2026).

---

## 1. Landscape table

| Name | Category | Language / runtime | License | Footprint | Single binary/container? | 2025–2026 health |
|---|---|---|---|---|---|---|
| **Ansible EDA / ansible-rulebook** | rule engine → runs Ansible | Python (rulebook CLI on Java-based Drools via `durable_rules`) | Apache-2.0 | ~250 MB RAM for the standalone rulebook CLI under normal load [[1]](https://github.com/ansible/ansible-rulebook) | Yes as a CLI/container; the full **AAP EDA controller** is heavy (8 GB+ RAM per component) [[2]](https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.5/html/rpm_installation/platform-system-requirements) | Active, but mid-transition: several event source plugins are moving out of the certified `ansible.eda` collection into a community-maintained `community.eda` collection; certified plugins require Red Hat's decision-environment images [[3]](https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.next/html/ansible_automation_platform_preview_release_notes/event_driven_ansible_ansible_eda_and_ansible_rulebook_changes) |
| **StackStorm** | full event-automation platform (rules, sensors, actions, ChatOps) | Python + RabbitMQ + MongoDB (+Redis/ZooKeeper) | Apache-2.0 | Heavy: RabbitMQ + MongoDB + Redis/ZooKeeper stack; HA deploys run 30+ pods [[4]](https://github.com/StackStorm/stackstorm-k8s/issues/24) | No — multi-service by design | Alive but slow: latest stable is v3.9.0 (Jan 2025), preceded by v3.8.1 (Dec 2023) — roughly one release a year, governed under Linux Foundation TSC [[5]](https://github.com/StackStorm/st2/releases) |
| **n8n** | general workflow automation, huge integration catalog | TypeScript/Node.js | **Sustainable Use License** (fair-code, not OSI open source) for core; `.ee.` files require a commercial Enterprise license [[6]](https://docs.n8n.io/sustainable-use-license/) | Practical minimum ~4 GB RAM / 2 vCPU for real use with Postgres; SQLite default doesn't hold up under concurrent writes [[7]](https://www.cherryservers.com/blog/n8n-self-hosting-requirements) | Single container, yes (with external Postgres for anything real) | Very active, huge community; core (non-`.ee.`) code is source-available but not resellable as SaaS |
| **Node-RED** | flow-based low-code automation | Node.js | Apache-2.0 | Light; runs comfortably on a Pi | Single container, yes | Active — 5.0 released June 2026, 4.x in maintenance mode, roughly annual majors tied to Node.js LTS [[8]](https://nodered.org/blog/2026/06/09/version-5-0-released) |
| **Windmill** | scripts→workflows→UI platform, has native approval steps | Rust (server/workers) + TS frontend | **AGPLv3** core, separate Enterprise Edition | Postgres is the only external dependency; ~1 vCPU/1–2 GB RAM per worker [[9]](https://automationatlas.io/tools/windmill/) | Docker Compose (3 files), small production-capable footprint | Active, funded startup (founded 2022); AGPL means self-host-and-modify is free, but re-exposing it as a hosted product to others requires a commercial license [[10]](https://www.windmill.dev/terms/2025-12-01) |
| **Kestra** | declarative, event-driven orchestration | Java/JVM | Apache-2.0 (Enterprise features closed) | JVM footprint; standalone mode runs all components in one process against one DB dependency [[11]](https://kestra.io/docs/architecture/deployment-architecture) | Standalone JAR/Docker container, yes | Active, well-funded; 1.0 shipped 2025, 27k+ GitHub stars, 1,700+ plugins [[12]](https://kestra.io/1-0) |
| **adnanh/webhook** | generic webhook→shell-command receiver | Go | MIT | Trivial; single static Go binary | Yes | Active community project (12k★, 552 commits), issues filed through Dec 2025; no formal release cadence but continuously used/patched [[13]](https://github.com/adnanh/webhook) |
| **OliveTin** | web UI + webhook front-end for predefined shell commands | Go | Apache-2.0 | "a few MB of RAM, barely any CPU" [[14]](https://www.olivetin.app/) | Yes, single binary/container | Active; regularly releases, positions itself explicitly as a safe button-front-end for scripts, including GitOps/webhook triggers |
| **ntfy** | push notification pub/sub (phone/desktop) | Go | Dual Apache-2.0 / GPLv2 | Tiny; single binary + optional SQLite | Yes | Very active, 29.7k★, integrates with Home Assistant/Uptime Kuma/Grafana out of the box [[15]](https://github.com/binwiederhier/ntfy) |
| **Apprise / apprise-api** | notification fan-out library (80+ services incl. ntfy, Gotify, Slack, email, SMS) | Python | MIT-family (BSD-2-Clause) | Minimal; library or a small Flask REST wrapper (`apprise-api`) | apprise-api: yes, single container | Actively maintained, 17k★, updates as recent as Aug 2026 [[16]](https://github.com/caronc/apprise) |
| **healthchecks.io (self-hosted)** | dead-man's-switch / cron & job monitoring, sends alerts, has webhooks | Python/Django | BSD-3-Clause | Django app + Postgres/MySQL; modest | Docker Compose (app + DB), not a single binary | Active, current image on Debian Trixie/Python 3.13/Django 6.0 [[17]](https://github.com/healthchecks/healthchecks) |
| **Gotify** | self-hosted push notification server w/ WebSocket | Go | MIT | Tiny; single binary | Yes | Active, 15.3k★, commits as recent as July 2026 [[18]](https://github.com/gotify/server) |
| **Grafana OnCall (OSS)** | on-call scheduling + incident escalation | Python/Django + Celery | AGPLv3 | Multi-service (app, Celery, Redis, Postgres) | No | **Deprecated**: read-only/maintenance mode since March 11, 2025; repo archives and Cloud API integration stops March 24, 2026. Only CVSS ≥7.0 fixes land. Grafana pushes users to Cloud IRM (not self-hostable) [[19]](https://grafana.com/blog/grafana-oncall-maintenance-mode/) |
| **Keep (keephq)** | AIOps alert correlation, dedup, workflows, single pane of glass | Python (FastAPI) + React | MIT core, proprietary `ee/` enterprise dir [[20]](https://github.com/keephq/keep/blob/main/LICENSE) | Multi-service (API, DB, workers); more than a single container out of the box | No | Active and growing fast: 12k★, 413 contributors, commits through July 2026, backed by a funded startup [[21]](https://github.com/keephq/keep) |
| **GoAlert** (Target) | on-call scheduling, escalations, SMS/voice via Twilio | Go + TypeScript | Apache-2.0 | Single binary + Postgres; Twilio needed for SMS/voice, otherwise minimal [[22]](https://github.com/target/goalert) | Yes (binary) + Docker image per release | Active, open-sourced and maintained by Target since 2021, still receiving releases |
| **Prometheus Alertmanager** | alert routing/dedup/silence (not itself an automation engine) | Go | Apache-2.0 | Tiny, single binary | Yes | Extremely mature, foundational CNCF project, still the de-facto alert router feeding all of the above via its generic `webhook_config` receiver |

---

## 2. Per-option analysis

### 2.1 Ansible EDA / ansible-rulebook — the natural fit, if used standalone

`ansible-rulebook` is the open-source rule engine (Apache-2.0) that consumes
events from *sources* (webhook, Alertmanager, watchdog, kafka, file, etc.),
matches them against *conditions* in a YAML "rulebook," and fires an Ansible
playbook or module in response. Crucially, **you do not need Ansible
Automation Platform (AAP) to use it** — the CLI/container runs standalone and
uses ~250 MB RAM under normal conditions [[1]](https://github.com/ansible/ansible-rulebook), a completely different order of
magnitude from AAP's EDA controller component (which wants 8 GB+ RAM like the
rest of AAP) [[2]](https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.5/html/rpm_installation/platform-system-requirements).

For this project's goals this is close to ideal: it is literally "Ansible
playbooks triggered by events," which is exactly the "runbook triggered by
alert" primitive the control stack needs, and it composes directly with the
Ansible/Terraform-via-Ansible core of the stack rather than being a second,
unrelated automation language.

The health caveat: Red Hat is actively reshuffling which event-source plugins
live in the certified `ansible.eda` collection versus a community-maintained
`community.eda` collection, and some certified sources will require Red Hat's
decision-environment container images to keep working [[3]](https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.next/html/ansible_automation_platform_preview_release_notes/event_driven_ansible_ansible_eda_and_ansible_rulebook_changes). For a homelab,
the practical answer is: pin to the community.eda collection and the plain
webhook/generic event sources, and treat the rulebook CLI (not the AAP EDA
controller) as the integration point. Alertmanager's `webhook_config` posts
JSON to any HTTP endpoint, and a rulebook's `ansible.eda.webhook` source is
exactly such an endpoint — a very short path from "alert fires" to "playbook
runs."

Approval gates are not native to ansible-rulebook; you'd build a thin gate
(e.g., require a `run_after: <ntfy topic ack>` webhook, or a Slack/ntfy action
button whose callback posts to a second webhook source before the destructive
rule fires) — this is glue work, not a blocker.

### 2.2 StackStorm — powerful but the wrong shape for homelab

StackStorm is the closest thing to a full "IFTTT for infrastructure" with
sensors, rules, workflows (Orquesta/Mistral-like), and ChatOps built in. But
its architecture requires RabbitMQ + MongoDB + Redis/ZooKeeper — a genuinely
heavy multi-service stack, with HA reference deployments running 30+ pods [[4]](https://github.com/StackStorm/stackstorm-k8s/issues/24).
Even single-box deployments carry that dependency chain. Release cadence has
slowed to roughly once a year (v3.9.0 in Jan 2025, v3.8.1 in Dec 2023) [[5]](https://github.com/StackStorm/st2/releases),
though it is not abandoned (Linux Foundation governance, active TSC). For a
one-operator homelab, StackStorm's operational overhead is disproportionate
to the benefit versus a slim rulebook + webhook receiver combo. **Not
recommended** for this stack.

### 2.3 n8n / Node-RED / Windmill / Kestra — general workflow engines as the "glue" layer

These four are not runbook engines themselves but excellent **orchestration/glue**
between "alert arrives" and "runbook executes," especially where you want a
visual flow, multi-step enrichment, or an approval UI that isn't worth
hand-rolling.

- **n8n**: richest integration catalog (PagerDuty, Slack, Alertmanager-shaped
  webhooks, ntfy, etc.) and has genuine human-in-the-loop primitives — a
  "Send and Wait for Response" node (Slack/Telegram/email/Teams) and a
  "Wait for Webhook" node that literally pause workflow execution until a
  human clicks Approve/Reject [[23]](https://docs.n8n.io/advanced-ai/human-in-the-loop-tools/). This is a strong match for the "approval
  gates" requirement. Caveat: it is **fair-code, not OSI-approved open
  source** — the Sustainable Use License lets you self-host and modify
  freely but forbids reselling n8n itself as a hosted service, and enterprise
  features (`.ee.` files) are proprietary [[6]](https://docs.n8n.io/sustainable-use-license/). For a homelab that's a non-issue,
  but it's worth being clear-eyed that this is not a copyleft/permissive OSS
  license. Practical resource ask is heavier than the other options — real
  use wants ~4 GB RAM + Postgres [[7]](https://www.cherryservers.com/blog/n8n-self-hosting-requirements).

- **Node-RED**: much lighter (fine on a Pi), Apache-2.0, actively developed
  (5.0 shipped June 2026) [[8]](https://nodered.org/blog/2026/06/09/version-5-0-released), and has a mature dashboard plus MQTT/HTTP nodes.
  Less polished for "wait for human approval" flows than n8n or Windmill
  (you'd build it from `http in`/`http response` pairs or a dashboard button),
  but very homelab-native (huge overlap with Home Assistant users).

- **Windmill**: has **first-class approval/suspend-resume flow steps** —
  a flow can pause, notify an approver (email/Slack/webhook) with a signed
  resume URL, and continue once approved, with configurable timeout [[9]](https://automationatlas.io/tools/windmill/) [[24]](https://www.windmill.dev/docs/flows/flow_approval).
  This is the most purpose-built "approval gate" primitive of any tool
  surveyed. AGPLv3, Postgres-only dependency, small worker footprint
  (~1 vCPU/1–2 GB RAM/worker) [[9]](https://automationatlas.io/tools/windmill/), Docker Compose in 3 files. AGPL only
  bites if you re-expose Windmill's UI to third parties as your own product —
  irrelevant for a single-operator homelab.

- **Kestra**: Apache-2.0 (fully permissive, unlike the other three), declarative
  YAML workflows, standalone mode runs everything in one process against one
  DB [[11]](https://kestra.io/docs/architecture/deployment-architecture), and is explicitly event-driven by design. JVM footprint is the main
  cost. 1.0 shipped in 2025 with strong momentum (27k★, 1,700+ plugins) [[12]](https://kestra.io/1-0).

### 2.4 Webhook receivers — the minimal-footprint option

- **adnanh/webhook**: a single static Go binary that maps an HTTP POST to a
  shell command, with rule matching on headers/payload/query — MIT licensed,
  12k★, still active (issues through Dec 2025) [[13]](https://github.com/adnanh/webhook). This is about as close to
  "zero footprint glue" as it gets: Alertmanager → `webhook` → `ansible-playbook`
  or `ansible-rulebook`, no database, no queue.
- **OliveTin**: adds a small web UI (and REST/webhook trigger surface) on top
  of the same "predefined shell command" idea, with an access-control layer
  so buttons can be safely exposed to less-trusted users — Go, Apache-2.0,
  "a few MB of RAM" [[14]](https://www.olivetin.app/). Its UI angle makes it a nice **TUI/web-UI-adjacent
  approval front end**: an alert could pre-populate an OliveTin action that a
  human then clicks to confirm, giving a manual approval gate almost for free.

Both are strong candidates for the literal "receive the webhook, exec the
runbook" leaf of the pipeline, with `ansible-rulebook`'s webhook source as the
more Ansible-native alternative when you want condition matching beyond what
`webhook`'s rule DSL offers.

### 2.5 Notification fan-out: ntfy, Apprise, Gotify

These solve the "tell the human something happened / ask for approval" leg,
not the runbook-execution leg.

- **ntfy**: simple `curl`/PUT-based pub/sub, has phone apps, priority
  levels, and **action buttons in the notification itself** (can call back
  into a URL) — genuinely useful for a one-tap "approve this playbook run"
  UX. Apache-2.0/GPLv2 dual license, very active (29.7k★) [[15]](https://github.com/binwiederhier/ntfy).
- **Apprise / apprise-api**: not a server with its own inbox — it's a
  fan-out layer that formats and sends a message to 80+ backends (ntfy,
  Gotify, Slack, Discord, SMTP, SMS gateways...) from one API call. Good as
  the single "notify" step any runbook or rulebook shells out to, so you
  don't have to hardcode a specific channel. MIT-family license, actively
  maintained (17k★, updated Aug 2026) [[16]](https://github.com/caronc/apprise).
- **Gotify**: simplest self-hosted push server (Go, MIT, single binary,
  15.3k★, commits through July 2026) [[18]](https://github.com/gotify/server) — a fine ntfy alternative if you
  prefer a slightly more minimal/older-school WebSocket model; Apprise talks
  to both.

### 2.6 Dead-man's-switch monitoring: healthchecks.io (self-hosted)

BSD-3-Clause, Python/Django, self-hostable via Docker Compose (app +
Postgres/MySQL) [[17]](https://github.com/healthchecks/healthchecks). This is the piece that catches *silent failures* — e.g.,
"my nightly backup didn't even attempt to run" — which Alertmanager-style
threshold alerting cannot see. It has its own webhook/integration
notification system, so it plugs into the same Apprise/ntfy/webhook fan-out
as everything else. Not single-binary, but it's a small, boring Django app —
proportionate for a homelab.

### 2.7 Alertmanager as the router, not the automation engine

Alertmanager itself should not be asked to run automation — its job is
dedup/group/route/silence, then hand off via a generic `webhook_config`
receiver [[25]](https://oneuptime.com/blog/post/2026-02-20-prometheus-alertmanager-routing/view). The recommended pattern from current field guidance:
annotate every alert with a `runbook_url`, route only the alerts you actually
want automated to a dedicated receiver, and keep `continue: true` on that
route so a human still gets paged even when automation also fires [[25]](https://oneuptime.com/blog/post/2026-02-20-prometheus-alertmanager-routing/view). That
receiver is where `adnanh/webhook`, OliveTin, or `ansible-rulebook`'s webhook
source sits. **Uptime Kuma** follows the identical shape — it POSTs a JSON
payload (monitor/status/message) to a webhook URL on state change [[26]](https://smartxhosting.uk/knowledgebase/1265/Setting-Up-Webhook-Notifications-in-Uptime-Kuma.html), so
the same receiver can serve both Alertmanager and Uptime Kuma as sources.

### 2.8 On-call escalation: Grafana OnCall is dead, alternatives exist

**Grafana OnCall OSS is deprecated**: read-only/maintenance mode since March
11, 2025 (only CVSS ≥7.0 security fixes), full archive and API cutoff on
March 24, 2026; existing installs keep running but lose Cloud Connection and
related services after that date, and Grafana is steering everyone to the
non-self-hostable Cloud IRM [[19]](https://grafana.com/blog/grafana-oncall-maintenance-mode/). **Do not adopt Grafana OnCall for new
homelab builds.**

Realistic self-hosted alternatives for the "escalate to a human, retry via
SMS/call if unacked" primitive:

- **GoAlert** (Target, Apache-2.0): single Go binary + Postgres, genuinely
  simple to self-host, does scheduling/escalation/SMS/voice via Twilio [[22]](https://github.com/target/goalert). Best match for the project's single-binary
  preference if you need actual phone/SMS escalation.
- **Keep (keephq)**: MIT core (enterprise features closed-source), positions
  itself as an AIOps alert-correlation + workflow platform, very active
  (12k★, commits through July 2026, well-funded) [[21]](https://github.com/keephq/keep). Multi-service
  (API + DB + workers), heavier than GoAlert, and more oriented at
  "correlate and dedupe alerts from many tools" than pure on-call paging —
  useful if alert noise (not escalation) is the actual homelab pain point.
- For SIGNL4-class "call my phone until I ack" behavior specifically, GoAlert
  is the closest OSS match; most other "alternatives" surfaced in this
  search (Cabot, OpenDuty) are effectively unmaintained/discontinued and
  should be avoided.

For a single-operator homelab, a full on-call escalation product is likely
**overkill** — ntfy's priority levels + action buttons, or Apprise fanning
out to phone push + email, cover "make sure a human sees this" without a
second Postgres-backed service to operate.

---

## 3. Recommendation

Compose, don't adopt a platform. None of the "all-in-one" reactive platforms
(StackStorm, Grafana OnCall, Keep) fit the footprint/health bar for this
project; the right shape is a thin chain of small, boring, single-purpose
pieces:

```
Alertmanager (webhook_config)  ─┐
Uptime Kuma (webhook)          ─┼─► adnanh/webhook  ──► ansible-rulebook (or ansible-playbook)
healthchecks.io (webhook)      ─┘        or                    │
                                     OliveTin (UI+webhook)      ├─► Apprise ──► ntfy / Gotify / email
                                                                 │      (notify + optional approval tap)
                                                                 └─► (for multi-step/approval flows) Windmill
```

1. **Runbook execution engine: `ansible-rulebook` (standalone CLI/container),
   not AAP.** It's Apache-2.0, ~250 MB RAM, and directly runs the same
   Ansible playbooks the rest of the control stack already uses — no second
   automation language to maintain. Pin to `community.eda` + generic
   webhook/alert sources to sidestep the AAP-only certified-collection churn.

2. **Ingress/router: keep it to Alertmanager's native `webhook_config`** (and
   Uptime Kuma's/healthchecks.io's built-in webhook notifiers) posting
   directly to `ansible-rulebook`'s webhook event source, or to
   `adnanh/webhook` in front of it if you want header/payload filtering with
   zero code. Both are single static-binary-class footprints.

3. **Approval gate:** for anything destructive, don't build a bespoke gate —
   either (a) use **ntfy's action-button callback** as the ack, gating a
   second-stage webhook that actually fires the rulebook rule, or (b) if the
   runbook has multiple steps and you want a real pause/resume UI, drop
   **Windmill** in front of the Ansible call for that one flow (AGPLv3 is a
   non-issue at homelab scale; Postgres is its only dependency). Don't adopt
   Windmill wholesale as the automation engine — use it selectively where its
   native suspend/approval step earns its keep over ntfy-button glue.

4. **Notifications: Apprise (or apprise-api container) as the single fan-out
   point**, feeding ntfy (preferred: has action buttons + good mobile apps)
   and/or Gotify. This keeps every runbook's "tell someone" step a single
   HTTP call regardless of which channel you're actually using this month.

5. **Dead-man's-switch coverage: self-hosted healthchecks.io** for "did the
   backup job even run," wired into the same Apprise/webhook fan-out as
   everything else.

6. **Skip for homelab scale:** StackStorm (multi-service, disproportionate
   ops burden for a one-operator setup), Grafana OnCall OSS (deprecated,
   archiving March 2026), Cabot/OpenDuty-class on-call tools (unmaintained).
   Treat GoAlert as the fallback only if genuine phone/SMS on-call escalation
   becomes a real requirement — it's the single-binary, Apache-2.0 option if
   so.

This composition keeps every piece independently swappable (the project's
stated preference for "thin glue over one big platform"), keeps steady-state
footprint in the tens-of-MB-to-low-hundreds-of-MB range rather than a
multi-service platform, and reuses Ansible as the one runbook language across
both the reactive path here and the Tower/AWX-equivalent scheduled path
described in the rest of the control-stack goals.

---

## Sources

1. [ansible/ansible-rulebook — GitHub](https://github.com/ansible/ansible-rulebook)
2. [AAP 2.5 RPM installation system requirements — Red Hat Docs](https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.5/html/rpm_installation/platform-system-requirements)
3. [Event-Driven Ansible ansible.eda and ansible-rulebook changes — Red Hat Docs](https://docs.redhat.com/en/documentation/red_hat_ansible_automation_platform/2.next/html/ansible_automation_platform_preview_release_notes/event_driven_ansible_ansible_eda_and_ansible_rulebook_changes)
4. [Use custom/external RabbitMQ and MongoDB · Issue #24 · StackStorm/stackstorm-k8s](https://github.com/StackStorm/stackstorm-k8s/issues/24)
5. [Releases · StackStorm/st2](https://github.com/StackStorm/st2/releases)
6. [Sustainable Use License — n8n Docs](https://docs.n8n.io/sustainable-use-license/)
7. [n8n Self-Hosting Requirements Guide (2026) — Cherry Servers](https://www.cherryservers.com/blog/n8n-self-hosting-requirements)
8. [Node-RED 5.0 released](https://nodered.org/blog/2026/06/09/version-5-0-released)
9. [Windmill 2026: OSS Internal Tools, AGPLv3, Free Self-Host — Automation Atlas](https://automationatlas.io/tools/windmill/)
10. [Windmill License terms (2025-12-01)](https://www.windmill.dev/terms/2025-12-01)
11. [Deployment Architectures in Kestra: JDBC and Kafka](https://kestra.io/docs/architecture/deployment-architecture)
12. [Introducing Kestra 1.0](https://kestra.io/1-0)
13. [adnanh/webhook — GitHub](https://github.com/adnanh/webhook)
14. [OliveTin — official site](https://www.olivetin.app/)
15. [binwiederhier/ntfy — GitHub](https://github.com/binwiederhier/ntfy)
16. [caronc/apprise — GitHub](https://github.com/caronc/apprise)
17. [healthchecks/healthchecks — GitHub](https://github.com/healthchecks/healthchecks)
18. [gotify/server — GitHub](https://github.com/gotify/server)
19. [Grafana OnCall OSS in maintenance mode: your questions answered — Grafana Labs](https://grafana.com/blog/grafana-oncall-maintenance-mode/)
20. [keephq/keep LICENSE — GitHub](https://github.com/keephq/keep/blob/main/LICENSE)
21. [keephq/keep — GitHub](https://github.com/keephq/keep)
22. [target/goalert — GitHub](https://github.com/target/goalert)
23. [Human-in-the-loop for AI tool calls — n8n Docs](https://docs.n8n.io/advanced-ai/human-in-the-loop-tools/)
24. [Suspend & Approval / Prompts — Windmill Docs](https://www.windmill.dev/docs/flows/flow_approval)
25. [How to Configure Prometheus Alertmanager Routing and Receivers — OneUptime](https://oneuptime.com/blog/post/2026-02-20-prometheus-alertmanager-routing/view)
26. [Setting Up Webhook Notifications in Uptime Kuma — SmartX](https://smartxhosting.uk/knowledgebase/1265/Setting-Up-Webhook-Notifications-in-Uptime-Kuma.html)
