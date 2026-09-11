# exocomp — ansible e2e rig (`.contrib/ansible-e2e/`)

Proves exocomp's ansible + reactive path against REAL lima Fedora VMs, the
same "verify real vs theory" principle as `.contrib/tofu-e2e/`. One daemon on
`0.0.0.0:8642`, one source checkout (`projects/infra`), one managed node
(`exocomp-node0`), and the reactive loop that turns a host joining a netgroup
into an ansible playbook run.

## The loop this proves

1. **Happy path** — a playbook runs through the one queue against a real VM
   and leaves a marker file (an independent side effect: a pass is a real
   pass, never exit-0-alone).
2. **Netgroup check** — a TCP check probes the VM's SSH forward; the sweep
   classifies the target address into the `lab` netgroup (`127.0.0.0/8`) and
   labels every event with it.
3. **Host-joins-netgroup trigger** — the VM stops (check → down → resolved
   event) and starts again (check → up → firing event labelled
   `netgroup=lab`); the `onboard-node0` rule matches `status == "firing" &&
   labels["netgroup"] == "lab"` and runs `plays/site.yml --limit node0` from
   the `infra` checkout automatically.

## Layout

```
lib/daemon.sh    throwaway daemon (same as the tofu rig)
lib/prepare.sh   start the node, re-derive the SSH endpoint, write the
                 per-run ssh_config + inventory into projects/infra
projects/infra/  the source checkout: plays/site.yml + ansible.cfg
                 (inventory.ini and ssh_config are per-run, gitignored)
run-e2e.sh       scenario driver (A01–A04)
```

## Run it

```sh
make build
.contrib/ansible-e2e/run-e2e.sh
```

The daemon runs the ENFORCING sandbox tier. The ansible jobs carry
`--egress 2222` — the lima node's loopback SSH forward — so the sandboxed run
reaches it: A01 (`playbook run --source infra --egress 2222`) and the
`onboard-node0` rule (`--egress 2222`) both write the marker through the
kernel sandbox. This is the G3 acceptance proof: a nonstandard-port inventory
host is reachable from the sandboxed ansible tier via the per-job egress grant
(empty by default = the port-22 posture).

The node's SSH port is pinned to `127.0.0.1:2222` via `ssh.localPort` in
`test/lima/node-fedora.yaml`, so the check target and the inventory survive VM
restarts. One instance of the template at a time.

## The demo

`lib/record-demo.sh` drives the web surface through the same beats (login →
sources → manual playbook run → job log → checks/netgroups → rules → stop the
VM → start it → the rule-fired job appears → marker verified on the VM) under
`playwright-cli`; see `.agents/brain/screenshots/ansible-e2e/`.
