# Terraform → Ansible inventory handoff

The handoff runs `tofu apply`, captures the workspace's declared outputs, and
feeds them to an Ansible play as its inventory — in one daemon, without ever
handing the fleet-facing play a state key. This is story 5 of the v1 acceptance
bar: the capability one tool holding both halves can make safe and a hand-rolled
`terraform output | jq` cannot.

For a workspace whose apply creates hosts that do not exist yet, the chain to
reach for is [`provision run`](provision-converge.md): the same apply and
capture, plus the bounded SSH wait and the onboarding step that stand between a
finished apply and a machine a play can reach.

## Governing rule — never expose raw state

The inventory is built from declared `output` blocks, **not** the state file. A
`tfstate` embeds every resource attribute in the clear, including provider
secrets and generated passwords, so it is never read for inventory. At apply
end exocomp runs `tofu output -json` inside the same sandboxed run that already
holds the backend bearer and the pbkdf2 state passphrase, keeps only the
**non-sensitive** outputs, and writes them to an inventory artifact. An output
marked `sensitive = true` is dropped before anything is written or logged.

## Shaping the inventory

Two conventions, chosen by output name:

- **Canonical — an output named `ansible_inventory`.** Its value is written as
  the inventory verbatim, so you shape hosts and groups in Terraform. Use the
  Ansible JSON/YAML inventory structure:

  ```hcl
  output "ansible_inventory" {
    value = {
      all = {
        children = {
          web = { hosts = { web1 = { ansible_host = aws_instance.web1.private_ip } } }
        }
      }
    }
  }
  ```

- **Fallback — no `ansible_inventory` output.** Every non-sensitive output
  becomes a var on the implicit `all` group. This is a valid but hostless
  inventory; declare an `ansible_inventory` output to target real hosts.

Mark anything secret `sensitive = true` and it never leaves the apply run:

```hcl
output "db_password" {
  value     = random_password.db.result
  sensitive = true
}
```

## Running the chain

```
exocomp handoff run <workspace> <playbook> [--vault-project P] [--run-secrets B] [--wait]
```

This defines a two-step step-ledger workflow (ADR 0008): step 1 is the apply
(with capture), step 2 is the play. Step 2 runs only if step 1 succeeded;
a failed apply leaves the play `blocked`, readable with:

```
exocomp handoff status <workflow-id>
```

The chain is driven by a running `exocomp serve` — its sequencer releases each
step, and after a crash it resumes the workflow from the ledger (a step the
crash interrupted is re-run; a step that already succeeded is not). Start the
daemon first; `handoff run` refuses when none is up.

`--run-secrets` binds a sealed-secret set as provider/`TF_VAR_` env for the
apply step only. `--vault-project` derives the play's vault password.

## What the play receives, and does not

The apply writes the inventory to `artifacts/inventory/<workspace>.json` under
the data dir (0600) and registers it as the `tf:<workspace>` inventory, so
`playbook run --inventory tf:<workspace>` reaches it too. The play consumes it
as a **copy in its own 0700 private directory**: the driver copies the artifact
in and points `--inventory` at the copy.

The play's process carries no backend bearer, no state passphrase, and no
`tfstate` — only the inventory (declared, non-sensitive outputs) and whatever
its own `--vault-project` / run-secret binding grants it. The capture holds the
state keys; the fleet-facing run never sees them.
