export const meta = {
  name: 'exocomp-persona-scenarios',
  description: 'Persona panel: surface uncovered real-world scenarios + desired flows',
  phases: [{ title: 'Personas', detail: '5 practitioner personas enumerate gap scenarios' }],
}

// Read-only research. No worktrees, no code edits — each persona returns a
// structured scenario list we synthesize and prioritize with the operator.

const CONTEXT = `
exocomp is a single static Go binary (a lightweight homelab/infra control plane) that shells out to ansible-core, opentofu, restic, sops/age, and git. WHAT IT DOES TODAY:
- SOURCES: a registered git repo (generated deploy key, auto-sync to a signed ref), a host-path, or an in-container path. Runs ansible playbooks and opentofu plan/apply from the checkout.
- EXECUTION: every run goes through ONE queue, sandboxed (nono/Landlock), success is exit-code-only, secrets redacted from logs/events. Concurrency keys serialize conflicting runs.
- OPENTOFU: an encrypted+locked HTTP state backend stored in SQLite, per-workspace bearer. Mode A = state-only (operator runs tofu on their laptop/CI, state lands here). Mode B = exocomp runs plan/apply server-side; plan is a sealed encrypted artifact, apply consumes exactly it.
- SECRETS: an age->DEK->sealed-row envelope; ansible-vault integration (multiple vault-ids); a "run-secret" plane that binds sealed secrets to a run as NAMED ENV so dynamic-inventory plugins and providers authenticate under the sandbox.
- REACTIVE: an alertmanager/webhook event -> CEL rules -> guardrails (cooldown, daily cap, dedup, silence/pause) -> approval gate (HMAC, TTL fail-closed) -> ntfy notification / a gated run.
- SCHEDULES: cron-driven tofu-plan drift checks and backups.
- BACKUPS: restic to NAS/S3, encrypted, keyring-downgrade refusal, a printed recovery kit, restore drills.
- SURFACES: a LAN web UI (token auth, a phone approval surface), a TUI (Bubble Tea + SSH), a /metrics endpoint with staleness gauges.
- NEW in the milestone building now (M3c): durable multi-step workflows with a step-ledger + crash resume; a CEL POLICY gate over tofu plan output (deny / require-approval); the Terraform->Ansible inventory handoff (capture declared tofu outputs -> materialize an inventory -> run a dependent play, without exposing raw state); and self upgrade/rollback with snapshot.
- HOST ONBOARDING: a seeded runbook pushes a fleet SSH key, creates the automation user, records known_hosts (backed up).

THE FIVE ACCEPTANCE STORIES WE ALREADY PLAN TO TEST FOR v1.0:
1. "Here is my git repo, go run my playbook regularly" (auto-sync a signed ref, a bad commit cannot reach the fleet).
2. "On event/alert, run X" (alert -> CEL rule -> guardrails -> optional approval -> run).
3. "Run my terraform regularly and notify me of drift" (scheduled plan/-refresh-only, resource-level drift, notified).
4. "Load my working directory into the server and run ansible/terraform" (host-path / in-container-path source).
5. "Run terraform, then trigger dynamic inventory FROM terraform for my ansible play and execute it."
`

const SCHEMA = {
  type: 'object',
  required: ['persona', 'topology', 'scenarios', 'top_gaps'],
  properties: {
    persona: { type: 'string', description: 'One-line who-you-are' },
    topology: { type: 'string', description: 'Concrete fleet/estate: nodes, clouds, OSes, providers, counts' },
    scenarios: {
      type: 'array',
      items: {
        type: 'object',
        required: ['title', 'frequency', 'trigger', 'flow', 'providers', 'maps_to', 'why_it_matters', 'exocomp_fit'],
        properties: {
          title: { type: 'string' },
          frequency: { type: 'string', enum: ['multiple-daily', 'daily', 'weekly', 'monthly', 'rare-but-critical'] },
          trigger: { type: 'string', enum: ['schedule', 'event-or-alert', 'manual', 'drift-detected', 'git-push', 'on-boot-or-recovery'] },
          flow: { type: 'string', description: 'The ordered flow the persona WANTS, step by step, in their words' },
          providers: { type: 'array', items: { type: 'string' }, description: 'Concrete tools/providers: proxmox, fly.io, digitalocean, cloudflare, hetzner, tailscale, raspberry-pi, k3s, docker, etc.' },
          maps_to: { type: 'string', description: 'Which of the 5 acceptance stories it extends, or "NEW GAP" if none cover it' },
          why_it_matters: { type: 'string' },
          exocomp_fit: { type: 'string', enum: ['strong', 'partial', 'missing-capability'], description: 'and if partial/missing, name the specific capability needed' },
        },
      },
    },
    top_gaps: { type: 'array', items: { type: 'string' }, description: '2-4 highest-value capabilities exocomp lacks for this persona, most important first' },
  },
}

const ASK = (who, brief) => `You are ${who}. Be a REAL practitioner, not a marketer — ground every scenario in how these tools are actually operated day to day, name concrete providers/CLIs, and mark anything speculative. Do NOT restate the five stories we already test; your job is to surface what they MISS.

${CONTEXT}

YOUR PERSONA BRIEF: ${brief}

Produce a realistic list of scenarios and use cases YOU hit that the five acceptance stories do not already cover (aim for 5-9 concrete scenarios; quality over quantity — each must be something a real person in your position does). For EACH: the exact flow you WANT (step by step, in your words, including the trigger, what exocomp should do, and how you want to be notified/approve), the concrete providers involved, whether it extends one of the 5 stories or is a NEW GAP, why it matters, and an honest exocomp_fit (strong / partial / missing-capability — name the missing capability). Think hard about the seams between local and cloud, about secrets/credentials for each provider, about DNS and TLS and object storage, about what happens on failure and recovery, and about the boring recurring chores that dominate real ops. Return ONLY the structured object.`

phase('Personas')
const personas = [
  ['a HOMELAB hobbyist who runs everything on bare metal at home',
   'You run a homelab: 3-5 mini-PCs / an old server / several Raspberry Pis, Proxmox VE for VMs and LXC, some bare Debian and one NixOS box, a Synology or TrueNAS NAS, Tailscale for remote access, self-hosted apps (Home Assistant, Jellyfin, *arr stack, Paperless, Vaultwarden) in Docker/compose or k3s. No public cloud, or only DNS. You value set-and-forget, reproducibility after a reflash, and being told BEFORE something breaks. Think about: node reprovision after an SD-card/SSD death, OS patching windows across heterogeneous nodes, container image update sweeps, certificate renewal for internal services, config backup/restore of app data, adding a new node, UPS/power events.'],
  ['a HYBRID personal-infrastructure operator spanning local + several clouds',
   'THIS IS THE HEADLINE CASE. Your estate: 4 heterogeneous local nodes (different CPUs/OSes — say an Intel NUC on Ubuntu, a ryzen box on Proxmox, a Pi4 on Raspberry Pi OS, an old Mac mini), 2 laptops (yours + a partner/second machine) that come and go, a few Fly.io machines (an app + a Postgres), 2-3 DigitalOcean droplets, Cloudflare for DNS + a couple of Workers + R2 + Tunnels, maybe a Hetzner box. You want ONE control plane that treats local and cloud uniformly where it can, and honestly where it cannot. Think hard about: a change that spans local AND cloud in one operation, machines that are intermittently online (laptops), per-provider credentials/tokens, DNS records that must follow a service when it moves between providers, promoting something from a laptop to a droplet, cost/teardown of idle cloud resources, an inventory that unions local SSH hosts with cloud dynamic inventories from Fly/DO, and disaster recovery of the WHOLE estate.'],
  ['a CLOUD-NATIVE indie/solo developer, Terraform-first, mostly serverless + small VMs',
   'You are a solo dev / tiny SaaS. Almost no hardware: Fly.io (apps + volumes + Postgres), Cloudflare (DNS, Workers, Pages, R2, Tunnels), DigitalOcean or Hetzner for a couple of always-on VMs, Neon/Supabase for managed Postgres, GitHub for CI. You live in Terraform/OpenTofu and wish config-management and secrets were as clean. Think about: ephemeral preview/staging environments spun up per branch and torn down, promoting staging->prod, rotating provider API tokens and app secrets on a schedule, seeding a fresh managed DB, wiring DNS+TLS for a new service, drift between what Terraform declares and what you hotfixed by hand in a provider console, and a one-command bootstrap of the whole stack for a new project.'],
  ['a PROSUMER/side-business SRE managing infra for a tiny team with real uptime needs',
   'You run infra for a 2-6 person startup or a monetized side project. A mix: a couple of Hetzner/OVH dedicated or DO droplets, a managed k8s or k3s cluster, object storage, a staging + prod split, and a small on-call rotation (just you and maybe one other). You care about approvals, an audit trail, backups you have actually restored, and being paged correctly. Think about: change approval and audit for prod, coordinated multi-step deploys (migrate DB -> deploy -> smoke test -> flip traffic, with rollback), scheduled DR restore drills, secret rotation with zero downtime, on-call escalation when a run fails or drift is dangerous, patching prod in a controlled window, and onboarding/offboarding a teammate (SSH keys, access).'],
  ['a PRIVACY- and SECURITY-focused self-hoster who is recovery-obsessed',
   'You self-host to own your data, on a small local fleet plus one or two cheap VPSes for offsite backup and a relay. You are strict about secrets hygiene, encryption at rest and in transit, minimal attack surface, and above all RECOVERY: you assume any node can die or be seized and you must rebuild from encrypted backups with a printed recovery card. Some networks are near air-gapped. Think about: full bare-metal recovery of a node and of exocomp ITSELF, encrypted backup fan-out to multiple destinations (NAS + offsite VPS + cloud object store + a github repo), periodic verified restore, secret rotation and revocation when a device is lost, certificate and SSH key lifecycle, offline/air-gapped runs, and detecting tampering/unexpected drift as a security signal not just a config nuisance.'],
]

const results = await parallel(personas.map(([who, brief]) =>
  () => agent(ASK(who, brief), { label: `persona:${who.slice(0, 24)}`, phase: 'Personas', model: 'opus', schema: SCHEMA })
))
return { personas: results.filter(Boolean) }
