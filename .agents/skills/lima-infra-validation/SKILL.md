---
name: lima-infra-validation
description: Prove exocomp's ansible and remote-execution behaviour against a real lima Linux VM instead of asserting it from source-reading — the repo's "verify real vs theory" principle applied to the paths that touch external hosts (SSH inventory, per-source venv interpreter, galaxy vendoring, exit-code truth). Use before claiming a change to the ansible/remote path works, or when a design doc asserts remote behaviour no unit test exercises.
---

# lima infra validation

A unit test proves the Go logic; it does not prove that exocomp reaches a real
SSH host, gathers facts, and returns the right exit code. This skill is how you
close that gap without touching the operator's homelab: drive exocomp
end-to-end against a disposable lima VM used as a genuine external ansible
target.

Reach for this when the claim under test lives on the boundary exocomp cannot
observe from source alone — inventory/SSH connection, the local-vs-remote
interpreter decision, galaxy collection vendoring, or the success/failure exit
code an agent will trust. A change that only moves in-process logic does not
need a VM; a change that reaches an external host is not proven until it has
reached one.

## What lima is and is not here

- It **is** a real Linux target for ansible: a real kernel, a real
  `/usr/bin/python3`, a real SSH daemon on a real (loopback-forwarded) port. A
  play that succeeds against it succeeded against a real remote host, and a
  side effect you can independently observe on the VM is real proof, not a
  mock.
- It is **not** exocomp's production runtime. exocompd ships in a
  `fedora-minimal` container (no `systemd` package, so no `systemd-run` binary
  and no user manager); a lima VM is a full OS. Anything that depends on the
  container's constraints — cgroup writability, the `UserNS=keep-id` posture,
  the absence of a user systemd — cannot be concluded from a lima run. Validate
  the *ansible/remote spine* here; validate *container-runtime* behaviour where
  the container actually runs.

## Non-negotiables

- **Re-derive the SSH port on every run; never cache it.** lima forwards SSH to
  a loopback port that is reassigned when the VM restarts, so a port captured
  in a previous session or written into a static inventory will silently point
  at nothing (or at a different VM). Derive it fresh each run from
  `limactl show-ssh`.
- **Do not force a venv interpreter onto a remote target.** exocomp only forces
  the source venv's python as `ansible_python_interpreter` when the inventory
  is its built-in localhost target (`internal/executor/ansible.go:444`, gated
  on `localOnly`, which comes from `localOnlyTarget` at `:369`). A
  controller-side venv path is meaningless on the remote and yields rc 127.
  When testing a `requirements.txt` source against a lima host, confirm the
  *remote* task ran under the VM's `/usr/bin/python3` and only the local leg
  used the venv — do not add an interpreter override that defeats this.
- **A registered inventory name resolves against the source checkout, not the
  process CWD.** `--inventory <name>` is looked up via `staticInventoryPath`
  (`cmd/exocomp/ansible.go:77`); a bare name resolved from the wrong root
  parses zero hosts. Worse, a zero-host play can still settle succeeded/exit 0
  (exocomp warns via `emitNoHostsOutcome`, but the code stays 0) — a mis-pointed
  inventory looks green. When validating inventory reach, assert a real host was
  contacted (facts gathered, a changed side effect), never exit 0 alone; pass an
  absolute inventory path if a registered name misresolves.

The full runbook — VM lifecycle, port derivation, source/inventory setup, the
requirements.txt-vs-remote check, galaxy vendoring, and the assertions that
distinguish a real pass from a green-but-empty one — is in
`references/runbook.md`. Read it when you run a validation, not before.
