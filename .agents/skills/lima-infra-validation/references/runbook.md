# lima validation runbook

A concrete procedure for proving an exocomp ansible/remote change against a
throwaway lima VM. Placeholders: the VM is `node0`; substitute your own
instance name. Keep exocomp's data dir short (a normal `$XDG_DATA_HOME`, not a
deep scratch path): the sandboxed tier redirects TMPDIR under each job's private
root and coordinates tools over AF_UNIX sockets there, and AF_UNIX addresses cap
at 108 bytes. A private root past that budget (108 minus the job and tool-socket
overhead — `MaxPrivateRootLen` in `internal/executor/sandbox.go`) makes the
sandbox probe degrade the whole deployment to the unsandboxed local tier. It
degrades *loudly* — the probe returns a reason naming the path — but the run is
no longer sandboxed, so keep the path short if you want the sandbox.

## 1. Bring the VM up

```sh
limactl start node0        # or `limactl start --name node0 <template>` to create
limactl list               # confirm it reports Running
```

Treat the VM as disposable state. Use a throwaway `XDG_DATA_HOME` for the
exocomp run so a validation never mutates the operator's real data dir.

## 2. Re-derive the SSH endpoint (every run)

The forwarded SSH port changes across VM restarts. Derive it now; do not reuse
a value from a prior session or bake it into a committed inventory.

```sh
limactl show-ssh --format=cmd node0
# -> ssh -o IdentityFile="…/_config/user" -o User=<u> -o Hostname=127.0.0.1 -o Port=<PORT> lima-node0
```

`show-ssh` is deprecated but still prints the live host, port, user, and
identity file — which is exactly what you need. The equivalent generated
`ssh.config` (`ssh -F <config> lima-node0`) carries the same live port if you
prefer it. Read the port from whichever you use *this run*; never a cached one.

## 3. Register a local-path source

No git or forge is needed — a path source works out of the box:

```sh
exocomp source add infra --path <local-tree> --kind hostpath   # or --kind volpath
```

`--kind hostpath` bind-mounts read-only; `volpath` lands in the data volume.
Both are proven paths (`cmd/exocomp/source.go`, flags at `:85`/`:87`).

## 4. Build a static inventory from the derived endpoint

Write the inventory with *this run's* host, port, user, and the lima identity
key from step 2. Prefer an absolute inventory path over a registered
`--inventory <name>` unless you have confirmed the registered name resolves
against the source checkout (see the CWD caveat in SKILL.md).

## 5. The requirements.txt vs remote check (the load-bearing one)

If the source carries a project-root `requirements.txt`, exocomp resolves a
per-source uv venv (hash-gated, per ADR 0011) and runs the venv's own
`ansible-playbook`. The trap: the controller-side venv python must **not** be
forced as the remote module interpreter.

Assert both halves after the run:

- The **remote** task executed under the VM's `/usr/bin/python3` (not a
  controller venv path). A forced venv interpreter on a remote host surfaces as
  **rc 127** — module interpreter not found.
- The **local** leg (if any) used the source venv python.

This composition (`requirements.txt` deps + remote SSH host) is correct only
because the interpreter is forced solely for the built-in localhost target
(`internal/executor/ansible.go:444`, gated on `localOnly` from `localOnlyTarget`
at `:369`). Do not add an interpreter override to "fix" a remote run — that
reintroduces the rc-127 failure.

Two adjacent gotchas:

- Declare only **extra** libraries in `requirements.txt` (e.g. `jmespath`),
  never the `ansible` meta-package — it conflicts with the pinned `ansible-core`
  and makes uv resolution unsatisfiable.
- If a stale exocomp binary is in play, a fresh rc-127 sighting may be the old
  build, not a regression. Rebuild before concluding.

## 6. Galaxy collections (when the play needs one)

Vendor collections through the queue, then run the play:

```sh
exocomp playbook vendor --requirements <requirements.yml>
exocomp playbook run …
```

`vendor` runs `ansible-galaxy collection install` into a per-source collections
cache dir (`cache/collections/<source>/<requirements-hash>`, derived from the
`--requirements` file's project root and content — `cmd/exocomp/vendor.go`), the
same hash-keyed layout the run path resolves into at claim time (ADR 0015);
`playbook run` appends `<per-source>:<shared>` to `ANSIBLE_COLLECTIONS_PATH`
automatically (see `ansibleEnv`, `internal/executor/ansible_env_test.go`). Prove
attribution by confirming the collection resolved only from the per-source
vendored path.

## 7. Assert a real pass, not a green-but-empty one

Exit 0 alone is not proof. A zero-matching-hosts play can settle succeeded — the
exit code stays whatever ansible returned, and exocomp only *warns*
(`emitNoHostsOutcome`, `internal/executor/ansible.go`) — so a mis-pointed
inventory looks identical to a real success. Require independent evidence that
the remote host was actually touched:

- facts were gathered from the VM, and
- an observable side effect exists on the VM — verify it directly, e.g.
  `limactl shell node0 -- <check>`, not by trusting the play's own summary.

Exit-code doctrine to confirm holds: success -> exit 0; a task failure on a
*reachable* host -> job settles failed and the ansible rc passes through as the
job exit code (`outcome`, `internal/queue/queue.go`; ansible-playbook returns 2
for a task failure). A run that cannot connect at all is a different failure from
a task that ran and failed — distinguish them. (An unreachable-tolerant run that
settled partial is a *degraded success*: it exits 0.)

## 8. Tear down

```sh
limactl stop node0
```

Discard the throwaway `XDG_DATA_HOME`. Report what was proven claim-by-claim
(which host was contacted, which interpreter ran each leg, the exit codes seen)
— not "it worked". Real-infra use beyond lima stays the operator's call.
