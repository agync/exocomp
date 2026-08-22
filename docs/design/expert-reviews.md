# Ansible & OpenTofu expert reviews (2026-08-22)

Two senior-practitioner reviews grounded in the actual code (not ADR aspirations),
run after ADR 0010 reset the source/credential model. They converge on one
finding, stated two ways.

## The convergent verdict

exocomp built a **correct, encrypted, properly-sandboxed execution core** for both
tools — but not a **runner**, because the credential plane that lets a run
authenticate to the outside world is missing, and the env allowlist that makes the
sandbox safe is the same wall that starves it:

- **Ansible**: the allowlist (`HOME/LANG/LC_ALL/PATH/TZ` + `ANSIBLE_*`) strips a
  dynamic-inventory plugin's `PROXMOX_PASSWORD`/`AWS_ACCESS_KEY_ID`, so today
  exocomp runs *static-inventory playbooks against hosts you can already reach*.
- **OpenTofu**: `tofuEnvAllow` is `HOME/LANG/LC_ALL/PATH/TZ` + `TF_VAR_*` from the
  daemon's own environ; a provider's `CLOUDFLARE_API_TOKEN`/`PROXMOX_VE_TOKEN`
  never arrives, so mode B can only run auth-free providers.

Both fixes are the *same shape* and fit the envelope model directly: a **sealed
secret set bound to a run, exported as named env** (provider env, plugin env, or
`TF_VAR_`). It is the missing binding, not new infrastructure. This binding is
independent of source locality — a source is a git repo, a bind-mounted host
path, or an in-container path (ADR 0010) — so the credential planes are the same
whether the tree is synced or mounted.

## Ansible — must-fix for v1

1. **Inventory-credential injection** — an `inventory` object binding sealed
   secrets to env names for the run; `--inventory` accepts a registered inventory,
   not only a path. (The biggest hole; a code fact, not a design gap.)
2. **Flag passthrough** — `--check --diff` (Ansible's plan-equivalent, all four
   personas asked), `--limit`, `--tags/--skip-tags`, `--start-at-task`, `-e`,
   `--forks`. Pure argv.
3. **`ansible.cfg` fidelity** — set the job `Dir` to the checkout root so the
   repo's `./ansible.cfg` (roles_path, vault_identity_list, callbacks) loads;
   HOME-redirect already (correctly) defeats `~/.ansible.cfg`.
4. **Multiple vault-ids + import an existing password** — map many `--vault-id`
   labels; allow importing a pre-existing vault password (not only HKDF-derived,
   which is the "dead daemon bricks my playbooks" brick).
5. Document the v1 connection-secret path: vaulted `ansible_become_password`/
   `ansible_password` in group_vars (secret in the repo vault, password from the
   envelope) — simpler than sealed-rows-per-group for repos that can self-vault.

## Ansible — v1.1+

JSON stdout callback → real task tree + per-host recap + `.retry` set; roles leg
(`ansible-galaxy role install -r` — the collections-only vendor ignores `roles:`)
+ private automation-hub auth; ADR-0010 fleet key / known_hosts TOFU / ProxyJump;
ad-hoc module runs; network_cli/winrm/docker/k8s connection matrix.

## OpenTofu — must-fix for v1

1. **Provider/var secret plane** — the Ansible twin: sealed rows bound
   per-workspace, exported as provider env and/or `TF_VAR_`, with per-workspace
   var sets. Without it mode B cannot run one real provider.
2. **Mode A actually exists** — `workspace add --state-only` emits the `backend
   "http"` block + bearer (reuse `writeBackendConfig` + `WorkspaceToken`). The
   headline adoption path is currently unbuilt.
3. **Workspace-collision docs** — the HTTP backend has no `tofu workspace`
   (named-workspace) capability; `terraform workspace new/select` hard-fails. One
   exocomp workspace = one state = one backend URL; environments are separate
   exocomp workspaces. Say so in the emitted mode-A block.
4. **Operator-arg passthrough** — `-target`, `-replace`, `-var-file`,
   `-refresh-only`; `opArgv` hardcodes `plan -out` / `apply -auto-approve`. At
   minimum `-var-file` + passthrough for v1; `workspace destroy` may be v1.1.
5. **Apply gate + CEL policy** — apply is unconditionally `-auto-approve` today;
   wire plan→approval→apply and the CEL pass over `tofu show -json` (destroy-count,
   protected addresses). Saved-plan staleness is enforced by tofu re-reading state
   (good); git-provenance refuse-if-tree-moved is v1.1.
6. **Offline lock-hash edge** — a `.terraform.lock.hcl` generated online carries
   `zh:` hashes; `filesystem_mirror` yields only `h1:`, so offline `init` fails
   verification. Populate the mirror with `tofu providers lock -fs-mirror` writing
   platform-correct entries — the no-egress DR drill depends on it.
7. **SQLite state store** — sound (lock-as-UNIQUE-row is more correct than S3
   conditional writes; ADR 0004 holds). Sharp edge is **unbounded growth**: every
   serial retained, large blobs, same DB as the queue, pruning designed-not-built,
   every backup copies all serials. Confirm WAL + busy_timeout so a large POST
   in-transaction doesn't starve the single writer; ship the pruning policy.

## OpenTofu — v1.1+

sops-encrypted tfvars; lineage reconciliation after `state push -force`; DELETE
verb; `-refresh-only` drift (true drift vs pending config) + structured diff;
`workspace destroy`; git-provenance refuse-if-moved.
