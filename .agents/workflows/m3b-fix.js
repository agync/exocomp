export const meta = {
  name: 'exocomp-m3b-fix',
  description: 'M3b-fix: credential run-path wiring + acceptance-story gaps',
  phases: [{ title: 'Fix', detail: 'single opus lane, interlocking cross-lane glue' }],
}
const REPO = (typeof args === 'object' && args && args.repo) ? args.repo : '/path/to/exocomp'  // args {repo}
const SCHEMA = {
  type:'object', required:['commits','summary','verified','concerns'],
  properties:{
    commits:{type:'array',items:{type:'object',required:['sha','title'],properties:{sha:{type:'string'},title:{type:'string'}}}},
    summary:{type:'string'}, verified:{type:'array',items:{type:'string'}}, concerns:{type:'array',items:{type:'string'}},
  },
}
phase('Fix')
const result = await agent(`Read <worktree>/AGENTS.md first, then .agents/workflows/m1-plan.md section "M3b-fix" (your task list) and docs/design/{v1-acceptance,expert-reviews}.md + docs/adr/{0009,0010}.md. Worktree: ${REPO}/.worktrees/m3b-fix; you own the whole worktree (single lane — the shared-file edits the parallel lanes avoided are now yours). Conventions: logical-unit commits via "git -C <worktree>", conventional titles <= 52 chars, NO trailers, explicit staging, no slop comments, never sudo. Pre-commit runs make check (go test -race ./..., ~2.5 min) on the index — never bypass; be patient. Do NOT run broad rm on /tmp. Every fix gets a test; reproduce where a behaviour is claimed.

These close the M3b integration glue + deferred must-fixes so the v1 acceptance stories can gate. Read the cited code before editing.

1. RUN-PATH CREDENTIAL WIRING (the critical one). internal/executor already has RunSecrets (runsecret.go) and the store has source/inventory/run_secret bindings (internal/store/source.go), and internal/executor/{ansible,tofu}.go accept RunSecrets. What is MISSING: a run does not carry its binding NAMES nor resolve them. Add a binding reference to queue.Spec + store.JobArgs (the shared job row), and in cmd/exocomp newAnsibleDriver/newTofuDriver (and the daemon's kindRouter in serve.go) RESOLVE the named run_secret set + an inventory's env_binding into executor.RunSecrets at execution — decrypting the sealed values via the envelope at run time, injected as named env for that job only. Prove end-to-end: a job bound to a run-secret set gets the value as env in the child; a dynamic-inventory run resolves its inventory's env binding; the value is NOT in the job row (only the binding name), NOT in events (redactor), and the run is sandboxed. This makes stories 1/3/4/5 authenticate.
2. workspace add --state-only: emit a ready-to-paste backend "http" block + bearer (reuse api.WorkspaceToken + the existing backend-config writer), with a comment stating one exocomp workspace = one state = one backend URL and tofu workspace new/select is unsupported. Test the emitted block parses and carries the right address+bearer.
3. host onboard <hosts>: a seeded runbook/command that pushes the fleet key, creates the ansible_user + sudoers, and records known_hosts into a store table that is backed up. Test against the lima node exocomp-node0 if reachable (it exists, stopped; start it); else a hermetic ssh-to-localhost proof.
4. Offline lock-hash: when populating the provider fs-mirror, run "tofu providers lock -fs-mirror <dir> -platform=..." so .terraform.lock.hcl carries h1: entries that verify offline. Wire into the tofu driver's offline path; test an offline init verifies against the mirror-generated lock.
5. tfstate pruning: a policy (keep last N serials + last M days) applied on state append or by a maintenance command; test old serials are pruned while history within the window is kept.
6. Banner reconciliation: the web dashboard (internal/web) must show MUTED (from internal/store notify_mute, jobs still run) distinct from PAUSED (reactive global silence). Align the web viewmodel with the TUI's model. Test the dashboard renders both states.
7. Listener: state backend stays loopback-only (api.Server on 127.0.0.1); the web+API bearer surface is the TLS LAN listener (ADR 0009). Ensure serve wires a separate non-loopback TLS listener for web/token surface distinct from the loopback state backend; the loopback backend never requires a LAN token. Test a LAN bind serves web+api-with-token while the state backend stays loopback.
8. setup step-4: persist the operator display name + the generated ntfy topic to the store/notify config and send the test push through the notify transport (or a clear no-transport message). Test the persistence.
9. Docs (invariant 8): operator pages under docs/operations for source/inventory/run-secret (the source model + credential binding), token/web (LAN auth), mute/metrics/setup; docs/log.md entries. Relative links, no host paths.

Verification (REAL output): make check; the run-path wiring end-to-end proof (a bound secret reaching a job's child env, absent from job row + events); workspace add --state-only emitted block; then run ./test/e2e/m2-gate.sh and (PATH="$HOME/.local/bin:$PATH", lima exocomp-node0) ./test/e2e/m1-gate.sh from the worktree and paste their real PASS lines. Host: go 1.26, tofu, ansible-playbook, restic, age, sops, podman, limactl, nono 0.74.0. Never sudo. Network to galaxy/registry may be intermittent — if an offline-dependent test can't fetch, note it and prove the logic hermetically.`,
  { label:'fix:m3b', phase:'Fix', model:'opus', schema:SCHEMA })
return { result }