export const meta = {
  name: 'exocomp-red-team',
  description: 'Adversarial security review of v1.0: attack the running system, rank findings',
  phases: [{ title: 'Attack', detail: '5 adversarial lenses against a real instance' }],
}
const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'
const COMMON = `You are a RED-TEAM security researcher attacking exocomp v1.0 (the tag v1.0.0 tree at ${REPO}). Build a real instance and ATTACK IT — do not just read code, attempt the exploit and report whether it actually worked. Ground every finding in a real repro against a real binary where possible; mark anything you could not execute as ANALYSIS-ONLY. Rank by REAL single-operator homelab risk (a solo operator running one container/host), not theoretical CVSS. This is READ-ONLY w.r.t. the repo: do NOT commit, do NOT modify tracked files — work in a scratch dir (mktemp), build with \`go build -o /tmp/... ./cmd/exocomp\` or \`make build\`, init a throwaway data dir under XDG_DATA_HOME in your scratch dir. Never sudo. Do not exfiltrate anything off the machine or hit external networks beyond what a normal run does.
Context: exocomp is a single Go binary running ansible/tofu through a sandboxed queue; an age->DEK secrets envelope (docs/adr/0003); OpenTofu HTTP state backend in SQLite with per-workspace bearer (0004); nono/Landlock job sandbox (0005); LAN token auth argon2id + self-signed TLS (0009); reactive webhook->CEL->guardrails->approval (HMAC/TTL); signed-ref sources with exocomp-owned allowed-signers + known_hosts pinning (0010, migration 0014); upgrade/rollback with snapshot; three-leg encrypted backups + recovery kit.`
const SCHEMA = {
  type: 'object', required: ['lens', 'findings', 'summary'],
  properties: {
    lens: { type: 'string' },
    summary: { type: 'string' },
    findings: {
      type: 'array',
      items: {
        type: 'object',
        required: ['title', 'severity', 'exploited', 'vector', 'repro_or_analysis', 'operator_risk', 'mitigation'],
        properties: {
          title: { type: 'string' },
          severity: { type: 'string', enum: ['critical', 'high', 'medium', 'low', 'info'] },
          exploited: { type: 'boolean', description: 'true = actually reproduced against a real binary; false = analysis-only' },
          vector: { type: 'string' },
          repro_or_analysis: { type: 'string', description: 'the concrete steps + what happened (real output if exploited)' },
          operator_risk: { type: 'string', description: 'realistic risk to a solo homelab operator, and preconditions' },
          mitigation: { type: 'string' },
        },
      },
    },
  },
}
const lens = (name, brief) => () => agent(`${COMMON}\n\nYOUR LENS: ${name}. ${brief}\nAttack thoroughly, then return ONLY the structured object: your lens, a summary, and ranked findings (most severe first; empty findings array if the surface is genuinely sound — say so honestly rather than inventing).`, { label: `red:${name}`, phase: 'Attack', model: 'opus', schema: SCHEMA })

phase('Attack')
const results = await parallel([
  lens('secrets-envelope', 'Attack the age->DEK envelope + recovery kit: evil-maid on the data dir, keyring/DEK version rollback, downgrade-refusal bypass, recovery-card/Shamir abuse, DEK or subkey extraction from a running process or the DB, sealed-secret AAD confusion, redaction bypass (a secret reaching an event/log/argv). Try to read a sealed secret you should not be able to.'),
  lens('daemon-trust-boundary', 'Attack the daemon: the LAN token auth (ADR 0009 — forge/replay a token, bypass argon2id, session-cookie theft, non-loopback bind without a token), the tofu state backend bearer (cross-workspace bearer reuse, unauth state read/write), the reactive webhook path token, and the nono/Landlock job sandbox (a job reading exocomp.db / host.key / the socket / another workspace, an egress the profile should deny).'),
  lens('reactive-approval', 'Attack the reactive+approval path: approval HMAC/TTL forgery or replay, gate bypass (run a gated/destructive action without approval), CEL rule injection or a rule that evaluates attacker-controlled data into an action, guardrail evasion (cooldown/daily-cap/dedup/silence/mute abuse to flood or to suppress a real alert), webhook body-signature bypass, drift-event spoofing.'),
  lens('source-supply-chain', 'Attack the source + supply-chain trust: defeat the signed-ref + exocomp-owned allowed-signers (migration 0014) — get an unsigned/attacker-signed ref accepted, or bypass verify via ambient git config; known_hosts/host-key pin bypass or a MITM the pin should stop; deploy-key exposure; uv/venv or galaxy/module resolution poisoning (a malicious dep into a run); plan-policy (CEL) bypass so a denied plan applies; tampering with the pinned-tool set.'),
  lens('upgrade-recovery', 'Attack upgrade/rollback + backup/restore: a malicious staged binary surviving verify, a rollback that discards or corrupts data, a forged/tampered backup manifest passing restore (HMAC), keyring-downgrade on restore, restoring attacker state, the ETXTBSY/exec path, and the recovery-kit trust (a forged card, a snapshot swap). Try to get a bad binary or bad state trusted.'),
])
return { lenses: results.filter(Boolean) }
