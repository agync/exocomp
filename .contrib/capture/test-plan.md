# Capture rig test plan

What each capture shows, the state behind it, and how a failure reads. The
verification in `lib/verify.sh` is the gate: the rig will not shoot a render
whose state it cannot assert.

## Stage A: the design renders

Seed is `lib/seed.sh`. Port 18642, TLS, dark theme.

| Render | Route | State asserted |
|---|---|---|
| `1a-shell-dashboard` | `/` | one source never synced, five targets down |
| `1b-checks` | `/checks` | `presence` 3 of 3 down, `web-health` 1 of 1 down |
| `1c-sources` | `/sources` | source `site` carries no sync timestamp |
| `1d-source-detail` | `/sources/site` | same, plus the detail shell |
| `1e-secrets` | `/secrets` | `restic-repo` and `ntfy-token` both present |
| `dashboard-dark` | `/` | dark media emulation active |
| `dashboard-light` | `/` | light media emulation active |
| `1f-vocabulary` | none | not a capture; left as committed |

Known divergence: `loopback` reads down rather than unobserved. See the README.

## Stage B: managed-node captures

Needs `lima` and the dummy managed node from `test/lima/node-fedora.yaml`, one
instance at a time because it fixes the SSH forward's host port at 2222.

Seed: start the node, wait for SSH, then drive the ansible surface the same way
`.contrib/ansible-e2e` does. The captures that need a live node are the inventory
and run pages with a host that actually answers, which stage A cannot produce.

Verification: the node is reachable on 2222 and `exocomp` reports it in an
inventory.

## Stage C: live k3s apply

Needs `lima` and the `exocomp-k3s` instance.

Seed: `.contrib/tofu-e2e` scenario S11, which registers the `b-k8s` project as a
workspace, declares egress to 6443, plans (exit 2, resources to create), applies,
and confirms the namespace and configmap through `kubectl`.

Verification: the plan exits 2, the apply succeeds, and `kubectl` sees the
namespace.

## How a failure reads

| Symptom | Cause |
|---|---|
| `reload returned 000` | the reload curled plain HTTP against a TLS daemon; it needs `-k` |
| `daemon produced no tokens.env` | the port was taken, so `serve` never bound |
| `check X: want [...], got [unobserved...]` | the probe sweep had not landed yet; the wait budget was too short |
| `python failed: <traceback>` | a verify expression is broken; the error is surfaced rather than swallowed |
| `could not reach <base>/login` | the self-signed interstitial was not accepted |
