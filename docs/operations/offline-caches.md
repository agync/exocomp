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
