# Persona scenarios — what the five acceptance stories miss

A five-persona practitioner panel (2026-08-22) was asked one question: given what
exocomp does today and the [five v1 acceptance stories](v1-acceptance.md), what
real-world scenarios do those stories *miss*? Each persona described a concrete
estate and the flows they actually want. ~40 scenarios came back; they collapse
into ~10 cross-cutting capabilities, and the striking result is how few distinct
primitives the whole surface needs — the same handful recur across every persona.

This doc records the synthesis and the scope decision it drove. The ranked build
consequences live in [v1.1-roadmap.md](v1.1-roadmap.md).

## The personas (estates, verbatim topology)

1. **Homelab hobbyist** — 3× N100 mini-PCs (Proxmox VE 8, VMs+LXC), a TrueNAS box
   (restic repo) + a Synology second target, 4 Raspberry Pis (2 in k3s, one Debian,
   one NixOS), Tailscale, Cloudflare for external DNS + DNS-01 only, Pi-hole/Unbound
   local DNS, step-ca internal TLS. Apps in docker-compose/LXC. Values set-and-forget,
   reproducibility after a reflash, being warned *before* things break.
2. **Hybrid personal-infra operator** *(the headline case)* — 4 heterogeneous local
   nodes (NUC/Ubuntu, Ryzen/Proxmox, Pi4, Mac mini), 2 roaming laptops on Tailscale,
   Fly.io (app + Postgres), 2-3 DigitalOcean droplets, Cloudflare (DNS + Workers + R2
   + Tunnels), a Hetzner cold-standby. Tailnet unions everything; wants ONE control
   plane over local + multi-cloud, honest where it can't unify.
3. **Cloud-native indie dev** — ~0 hardware. Fly.io, Cloudflare (DNS/Workers/Pages/R2/
   Tunnel), 2 always-on VMs (Hetzner + DO), Neon + Supabase managed Postgres, GitHub
   CI. OpenTofu-first; provider auth sprayed across GitHub Actions secrets + .env.
4. **Prosumer/small-team SRE** — 2× Hetzner dedicated (k3s prod) + Storage Box, 3 DO
   staging droplets, self-hosted Postgres + warm replica, Cloudflare, restic to B2 +
   Storage Box, staging/prod workspaces, two-person on-call (ntfy today, PagerDuty on
   the wishlist).
5. **Security/recovery-obsessed self-hoster** — 3-4 mini-PCs (Proxmox + CoreOS/k3s)
   split across a trusted and a near-air-gapped VLAN, TrueNAS restic REST-server, a
   Hetzner WireGuard/headscale relay (home is CGNAT), a $5 offsite restic box, B2,
   a private git repo holding sops-encrypted configs + the escrowed recovery kit,
   step-ca PKI. Assumes any node can be lost or seized.

## The ten cross-cutting capabilities (ranked by breadth × leverage)

Personas column = how many of the five raised it independently.

| # | Capability | Personas | Unlocks | Gap vs today |
|---|---|---:|---|---|
| 1 | **Post-run health/verification gates** — success as more than an exit code: HTTP/DNS/TLS/metric/app-assertion probes that can *fail* an exit-0 run and gate a workflow | 5 | health-gated rollout, DNS cutover, blue/green, post-apply verify, verified restore, cert-serve check | correctness gap — `tofu apply` exit 0 ≠ service reachable |
| 2 | **Generic governed CLI run type** — `wrangler`/`flyctl`/`doctl`/`gh`/`kubectl`/`helm` as first-class runs under the queue + sandbox + run-secret + approval plane | 2 (strong) | every cloud/hybrid deploy that isn't ansible/tofu | today smuggled through an ansible `shell` task → escapes governance |
| 3 | **Non-tofu drift + time/deadline signals** — cert-expiry ladders, ansible `--check` drift, container-image-digest drift, host tamper (AIDE), RPO gauges | 4 | the "warn me *before* it breaks" core | only tofu-state drift; no notion of "days until X" |
| 4 | **Secret/credential rotation lifecycle** — mint → reseal(versioned) → canary → promote → revoke-old; zero-downtime dual-valid; fan-out to external sinks | 4 | the #1 recurring security chore | the store *seals* values, never *rotates* them |
| 5 | **Rolling/batch execution strategy** — serial host walk, health-gate between, halt-on-first-failure, maintenance-window time-box | 3 | patch sweeps, blue/green | queue serializes conflicts but can't "walk these, stop if one fails" |
| 6 | **App-level / logical backup + verified restore** — pg_dump/managed-DB backups, quiesce hooks, multi-destination fan-out w/ per-destination staleness, restore-drill that asserts the *app* returns | 4 | crown-jewel data (Postgres) restic can't walk | restic filesystem only; drills check the repo, not the app |
| 7 | **Control-plane self-DR** — rebuild exocomp *itself* from the printed card (its SQLite state backend, sealed secrets, ledger, deploy keys, fleet re-key); rehearsable drills | 2 (rare-critical) | recovering the thing that holds all the keys | backups cover *data*, not the control plane |
| 8 | **Recovery/reprovision workflows + host-key re-pin** — reflash → re-onboard *into a known_hosts conflict* → configure → restore data → verify, as one resumable workflow | 3 | node death → back to declared state | onboarding assumes a pristine first touch |
| 9 | **Ephemeral env lifecycle** — paired create/destroy keyed on an external id (PR#), TTL, orphan-reaper, live registry | 2 (high-freq) | per-PR previews without leaked cloud spend | create-on-event is easy; teardown/TTL/GC unmodeled |
| 10 | **Unified multi-source inventory + presence-gated runs** — union static-local + Fly/DO dynamic inventories w/ reachability filter; converge roaming laptops when they *appear* | 1 (strong) | "do X to everything online right now"; roaming machines | story 5 is single-source; cron against roaming machines = false-failure noise |

Team-oriented extras from the SRE — incident/escalation with tiered on-call, break-glass
runs, and audit-evidence export — are real but narrower; incident/escalation overlaps the
already-planned [GoAlert post-v1 work](../../.agents/workflows/post-v1-plan.md).

### What the headline (hybrid) estate points at hardest

The hybrid operator's own top gaps were **#2 (generic CLI — Fly/Cloudflare/DO deploys
are not ansible/tofu)** and **#10 (unified inventory + presence-gating the laptops)**.
These are the sharpest "one control plane across all of it" gaps and rank high in v1.1.

## The scope decision (2026-08-22)

Operator decision: **hold the v1 line, with one principled exception and four cheap folds.**

- **v1 stays** = the five acceptance stories reliable + M3c (workflows, plan policy,
  TF→Ansible handoff, upgrade/rollback) + the M3c pass-3 dependency & run-lifecycle work.
- **Pulled INTO v1 on correctness grounds: capability #1, the post-run health/verification
  gate.** The M3c workflows we already ship (chains, and any deploy/cutover) are unsafe
  with exit-code-only success — all five personas said so independently. A minimal probe
  step (a ledger step whose success is a declared HTTP/DNS/TLS/exit/metric check) makes
  the workflows we are *already building* safe. This is not gold-plating; it is the
  correctness of a feature already in scope.
- **Folded into v1 as small, adjacent additions** (each hardens an existing capability,
  not a new subsystem):
  1. **Sensitive tofu output → sealed secret sink** — the secrets twin of story 5.
  2. **Backup quiesce pre/post hooks** — free once the pass-3 hook mechanism exists.
  3. **Ansible `--check` drift signal** — extends story 3's structured-drift plumbing to
     the Ansible surface (90% of a homelabber's fleet).
  4. **Host-key re-pin runtime detection/quarantine** — extends the `ErrKnownHostChanged`
     primitive already built in M3b maintenance; the CLI re-pin exists, the runtime
     detect-and-quarantine + phone approval is the delta.
- **Everything else → the ranked [v1.1 roadmap](v1.1-roadmap.md)**, topped by #2/#3/#4.

The reasoning: v1's bar is *core reliability of the five stories*, not breadth. The
panel's value is not "build all this now" — it is a validated, prioritized picture of
where exocomp goes after v1, plus the one correctness fix the workflows genuinely need.
