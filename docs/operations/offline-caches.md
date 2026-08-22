# Offline dependency caches

Disaster recovery must not depend on Ansible Galaxy or `registry.opentofu.org` being
reachable (design/amendments.md item 8). Two caches, both inside the 0700 data dir and
both covered by the ordinary backup snapshot, make that true.

## Ansible collections

`exocomp playbook vendor` installs collections into `<data dir>/cache/collections`
through the queue, needing network access for whatever the cache does not already hold:

```
exocomp playbook vendor --requirements requirements.yml
```

`requirements.yml` is an ordinary `ansible-galaxy` requirements file:

```yaml
collections:
  - name: community.sops
```

Every `exocomp playbook run` appends that cache directory to `ANSIBLE_COLLECTIONS_PATH` —
appends, not replaces, so an operator-set `ANSIBLE_COLLECTIONS_PATH` still applies. A
playbook needs no flag, no environment variable, and no network to use a vendored
collection; it only needs to have been vendored once.

`community.sops` is worth vendoring on every install: the design itself depends on it
([disaster-recovery.md](disaster-recovery.md)'s S3 credential story assumes sops-encrypted
files are decryptable offline, and `ansible-galaxy collection install` warns, harmlessly,
that a path outside its configured collections paths "will not be picked up" — that
warning describes running `ansible-galaxy` directly, not a playbook run through
`exocomp`, which sets the path itself).

## Python dependencies

A source that declares Python dependencies (`requirements.txt`, or a
`pyproject.toml` with PEP 621 / `[dependency-groups]` deps) resolves into a
per-source uv venv at `<data dir>/cache/python/<source>/<manifest-hash>`
([ADR 0011](../adr/0011-project-dependency-resolution.md)). uv's own download
cache lands at `<data dir>/cache/python/uv-cache`, so both travel with the
backup.

**The resolver runs under the job sandbox, not the daemon.** uv resolves and
installs a project's declared deps, and a manifest can steer that — a
`pyproject.toml` build backend is arbitrary code, run at install time, derived
from an untrusted source. So when the sandbox tier is in effect the uv
invocation is wrapped in the same nono launcher every job runs under
([ADR 0005](../adr/0005-job-sandboxing-nono.md)): uv is granted write on its
venv and cache, read on its tool tree and the manifest, and HTTPS to fetch
packages, and is **denied the data dir, `host.key`, `keyring.age`, and the
database** by the same profile guard a run's grants pass through. A resolve that
tries to read a denied path fails closed, leaving no half-built venv. A host
that cannot enforce the sandbox resolves in the daemon context, loudly, the same
way any job degrades there.

## OpenTofu providers

`workspace plan`/`apply` sets `TF_PLUGIN_CACHE_DIR` to `<data dir>/tofu/plugin-cache`, so
a provider a previous run already downloaded is not fetched again.

That cache alone does not make a run network-independent: OpenTofu queries the
registry's discovery document to resolve a provider even when a cached copy would
already satisfy the request, so a plain `TF_PLUGIN_CACHE_DIR` run still needs a route to
`registry.opentofu.org`. The driver (`internal/executor/tofu.go`) also supports an
`OfflineProviders` mode that restricts resolution to that cache directory alone, with no
registry method declared at all — one directory serves both roles, since tofu's
filesystem-mirror method reads the exact layout `TF_PLUGIN_CACHE_DIR` already writes.

A lockfile written online carries only `zh:` (registry) hashes, which the filesystem
mirror cannot satisfy with no network, so an offline `init` would fail verification against
it. In `OfflineProviders` mode the driver first runs `tofu providers lock -fs-mirror
<cache> -platform=<current>`, which writes platform-correct `h1:` hashes into
`.terraform.lock.hcl` sourced from the mirror alone — the hashes the following offline
`init` can verify.

That mode is proven end to end (`TestTofuPlanRunsOfflineFromPluginCache`,
`internal/executor/tofu_offline_test.go`: one online apply, then a network-denied init and
plan from the same cache) but is not yet wired to a workspace CLI flag — a restore-drill
command exercising it end to end is follow-on work, not shipped here.

## Verifying offline resolution

Both proofs run for real, in a network-denied `unshare -rn` user+network namespace (no
sudo; user namespaces are the mechanism, verified on the target host):

```
go test ./internal/executor/... -run 'OfflineFromVendoredCollection|PlanRunsOfflineFromPluginCache' -v
```

Each test brings up whatever it needs (a real `ansible-galaxy`/`age`/`sops` fixture, or
exocomp's own HTTP state backend) inside the child namespace itself — a namespace's
loopback is its own, unreachable from any other namespace — and asserts a real network
dial to the registry it depends on fails before trusting the result.
