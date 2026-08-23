# Ansible-first bootstrap (`bootstrap run`)

`exocomp bootstrap run <playbook> <workspace>` composes the mirror of the
[Terraform→Ansible handoff](terraform-ansible-handoff.md): the ansible **play
runs first**, then `tofu apply` runs against the workspace, as one step-ledger
workflow. It is the ordering for a persona where Ansible bootstraps nodes and
Terraform then manages resources on top of them — `kubeadm` brings up a control
plane, then the `kubernetes`/`helm` providers manage cluster objects.

This is not a new engine. The [Sequencer](../adr/0008-step-ledger-over-dbos.md) has
always run steps in list order; every shipped verb happened to put `tofu apply`
first. `bootstrap run` is the CLI verb that composes `[ansible-play, tofu-apply]`
instead. The apply runs only if the play succeeded; a daemon that dies mid-chain
resumes it from the ledger, exactly as the handoff does.

## The chain

```
exocomp bootstrap run site.yml wsp --inventory hosts --wait
```

Two steps, in order:

1. **ansible-play** — the bootstrap play, run against `--inventory`. Because the
   apply has not run yet, there is no materialized `tf:<workspace>` inventory to
   build the play's hosts from, so the play targets an inventory you supply
   (`--inventory <path|name>`, or localhost by default).
2. **tofu-apply** — `tofu apply -auto-approve` against the workspace, gated on the
   workspace's tofu concurrency key like every other apply.

A running `exocomp serve` drives the chain: it releases step 1, and step 2 only
once step 1 reached succeeded. Without a daemon the command refuses rather than
leaving a chain that will never progress.

## Phase 0: the handoff between the steps is manual

This first cut runs the two steps in order and **captures nothing** from the play
into the apply. The apply reads its own pre-existing credential — bind it with
`--run-secrets <name>` exactly as the handoff binds one, e.g. a `KUBECONFIG`-shaped
run-secret the operator sealed ahead of time, or the apply's own `~/.kube/config`
via a standing binding.

What Phase 0 does **not** do: it does not seal a play-produced fact (the
`kubeadm` kubeconfig, an `admin.conf` fetched off a control-plane node) and hand
it to the apply automatically. That capture-and-bind hook is the next phase of
[bootstrap-first](../design/bootstrap-first.md); until it lands, a credential the
play produces reaches the apply only if you seal it and bind it yourself.

## Flags

| Flag | Effect |
|---|---|
| `--inventory <path\|name>` | Inventory for the bootstrap play (default: localhost). |
| `--vault-project <p>` | Derive the play's ansible-vault password from the envelope. |
| `--run-secrets <name>` | Bind a run-secret set as provider/`TF_VAR_` env for the **apply** step. |
| `--source <s>` | Wrap the chain in a registered source's `.exocomp/hooks.yml` lifecycle — pre-run hooks before the play, post-run hooks and the health gate after the apply — identically to `handoff run --source`. |
| `--require-approval` | Gate the destructive chain: refuse to start unless `--assume-yes` is passed. |
| `--timeout <d>` | Per-step timeout (0: no limit). |
| `--wait` | Follow the workflow to a terminal state. |
| `--output json` | Emit the wire envelope for the started workflow instead of prose. |

`exocomp bootstrap status <workflow-id>` prints the chain's steps from the ledger.

### Gating the destructive apply

The chain ends in a destructive `tofu apply`. `--require-approval` requires the
operator to pre-approve it with `--assume-yes` rather than the chain starting
unprompted; a non-interactive session with neither is refused, not blocked on a
prompt.

Note the Phase-0 boundary: this is a **confirmation at the CLI**, not a durable
per-step approval the daemon holds mid-chain. The Sequencer releases a gated
workflow step on its own, so an approval recorded on step 2 would not stop it —
enforcing a runtime, mid-chain approval for a composed chain needs Sequencer
support and is out of scope for Phase 0. The CLI confirmation is the guardrail
this phase ships.

## Machine output

```
exocomp bootstrap run site.yml wsp --output json
```

emits a success envelope whose `data` carries the workflow id and the ordered
step kinds, so a caller can confirm the chain is ansible-first without parsing
prose:

```json
{"schema_version":1,"kind":"bootstrap.workflow","data":{"workflow_id":"bootstrap:wsp:...","workspace":"wsp","playbook":"/abs/site.yml","steps":["ansible-play","tofu-apply"]}}
```

## Not to be confused with

The [Bootstrap](bootstrap.md) operations page covers first-run install and the
setup wizard — a different sense of the word. This page is the `bootstrap run`
verb: an ansible-first execution chain.
