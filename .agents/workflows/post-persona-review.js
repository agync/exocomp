export const meta = {
  name: 'exocomp-persona-review-v1',
  description: 'Re-run the four named personas against shipped v1.0: what works, what is still missing',
  phases: [{ title: 'Review', detail: 'Marcus / Priya / Tom / Elena vs v1.0' }],
}
const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'
const COMMON = `You are re-running a persona adoption review of exocomp, now that v1.0 has SHIPPED (tag v1.0.0 at ${REPO}). Read docs/design/user-feedback.md FIRST to recall the four personas and their ORIGINAL asks, and docs/design/persona-scenarios.md + docs/design/v1.1-roadmap.md for the later panel. Then judge the SHIPPED v1.0 honestly against your persona's needs — read the actual capabilities (cmd/exocomp help surface, docs/operations/*, the ADRs), don't guess. Be the same demanding practitioner as the M-series reviews: credit what now genuinely works, name what is still missing or awkward, and rank your top remaining asks. Ground claims in what v1.0 actually ships (the five acceptance stories, M3c capabilities, the deps/hooks/health-gate, upgrade/rollback). READ-ONLY: do not modify or commit anything.
v1.0 ships: git/hostpath/volpath sources with signed-ref + exocomp-owned allowed-signers; ansible+tofu through one sandboxed queue; encrypted state (Mode A/B); step-ledger workflows; CEL plan-policy; TF->Ansible inventory handoff; tofu drift + ansible --check drift; pre/post-run hooks + health/verification gate; per-source uv venvs + module cache; upgrade/rollback; three-leg encrypted backups + recovery kit + quiesce hooks; reactive webhook->rule->guardrail->approval->run; LAN web + TUI + SSH; /metrics staleness gauges.`
const SCHEMA = {
  type: 'object', required: ['persona', 'verdict', 'now_works', 'still_missing', 'top_asks'],
  properties: {
    persona: { type: 'string' },
    verdict: { type: 'string', description: 'would you adopt v1.0 now? one honest paragraph' },
    now_works: { type: 'array', items: { type: 'string' }, description: 'original asks v1.0 genuinely satisfies' },
    still_missing: { type: 'array', items: { type: 'string' }, description: 'original asks still unmet or awkward in v1.0' },
    top_asks: {
      type: 'array',
      items: {
        type: 'object',
        required: ['ask', 'why', 'maps_to'],
        properties: {
          ask: { type: 'string' },
          why: { type: 'string' },
          maps_to: { type: 'string', description: 'a v1.1-roadmap item (R1-R10 / A-D) or "NEW"' },
        },
      },
    },
  },
}
const persona = (name, who) => () => agent(`${COMMON}\n\nYOU ARE ${name}: ${who}\nReview shipped v1.0 as ${name}. Return ONLY the structured object.`, { label: `persona:${name}`, phase: 'Review', model: 'opus', schema: SCHEMA })

phase('Review')
const results = await parallel([
  persona('Marcus', 'the homelab hobbyist from user-feedback.md — bare-metal, set-and-forget, reproducibility-after-reflash, warn-me-before-it-breaks. Judge v1.0 against your original review asks and the homelab scenarios.'),
  persona('Priya', 'the prosumer/small-team SRE from user-feedback.md — real uptime, approvals + audit, on-call/escalation, DR drills, secret rotation. You care about the second-transport-on-unacked + synthetic-page asks. Judge v1.0 and note what still needs GoAlert.'),
  persona('Tom', 'the cloud-native/indie developer from user-feedback.md — Terraform-first, Fly/Cloudflare/DO, ephemeral envs, token rotation, non-ansible/non-tofu CLIs. Judge v1.0; be honest about the generic-CLI (R1) and rotation gaps.'),
  persona('Elena', 'the security/privacy-and-recovery-focused self-hoster from user-feedback.md — secrets hygiene, encrypted multi-destination backups, verified restore, air-gapped runs, revoke-on-device-loss, control-plane self-DR. Judge v1.0 against your recovery-obsessed bar.'),
])
return { personas: results.filter(Boolean) }
