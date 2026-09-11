# ansible-e2e test plan

Faces: CLI (A01–A04), web (the demo recording, `.agents/brain/screenshots/ansible-e2e/`).
Node: `exocomp-node0` (rootless Fedora 44, `test/lima/node-fedora.yaml`,
SSH forward pinned to `127.0.0.1:2222`).
Daemon: throwaway, `EXOCOMP_SANDBOX=disabled` (see README for why), data at
`/tmp/exocomp-throwaway/ansible-e2e`.

## A01 · happy path — playbook run through the queue

`exocomp playbook run plays/site.yml --source infra` enqueues an ansible job;
it settles succeeded (exit 0) and the marker file exists on the VM with the
expected content (`limactl shell exocomp-node0 -- cat
/tmp/exocomp-e2e/marker.txt` == `hello-exocomp-ansible`). A green-but-empty
play (zero hosts) is not a pass — the marker is the independent proof.

## A02 · source surface

The `infra` source is registered (path source, read-only), its checkout holds
`plays/site.yml`, and `ansible.cfg` resolves `inventory.ini` — the web Sources
page lists the discovered playbook.

## A03 · netgroup check

The `lab` netgroup covers `127.0.0.0/8`. The `node0-ssh` TCP check probes
`127.0.0.1:2222`; the check state flips up/down with the VM, and every check
event carries `labels.netgroup == "lab"` + `labels.target == "node0"` (a
non-covered address would read `away`).

## A04 · host-joins-netgroup triggers the playbook (RUN STANDALONE)

Run as `--scenario A04`, not in the back-to-back matrix: the matrix's A03 just
raised the SAME check condition (`check:node0-ssh:node0`), and the reactive
dedup window (5m) correctly suppresses A04's re-fire as a duplicate. Standalone
it is deterministic.

With the VM stopped, the check holds down (departure → resolved event). On
`limactl start` the join is declared after the 3-probe hysteresis (firing
event, `netgroup=lab`) and the `onboard-node0` rule matches
(`event.status == "firing" && event.labels["netgroup"] == "lab"`), queueing
`plays/site.yml --limit node0` from `infra`. The job settles succeeded and the
marker is written. The event row names the rule; the job's trigger reads
`event`, runbook `rule:onboard-node0`.

## Demo beats (web recording)

1. Token login (cert fingerprint) · 2. Sources → `infra` → manual playbook run
→ job log · 3. Checks page (node0-ssh, netgroup=lab) · Netgroups page ·
4. Rules page (onboard-node0) · Events page · 5. Stop the VM → check down ·
6. Start the VM → join → the rule-fired job appears → job log → marker verified
on the VM.
