# Pre/post-run hooks and the health gate

A source can wrap its run in lifecycle steps: ordered `pre_run` and `post_run`
shell steps, and a post-run health/verification gate whose success is a declared
probe — an HTTP status, a DNS resolution, a TLS handshake with a not-after
floor, or a numeric metric threshold — not merely a process exit code. A run of
such a source becomes one step-ledger workflow:

```
[pre_run...]  ->  the play  ->  [post_run...]  ->  the gate
```

Each entry is a queue job the daemon's sequencer releases in order, running the
next only once the previous one succeeded. So a `pre_run` step that fails leaves
the play **blocked** — it never runs — and a gate probe that fails marks the
whole run **failed** even when the play itself exited 0. That last property is
the point: `tofu apply` or an ansible play exiting 0 does not mean the service
is reachable, and the gate is what turns "the tool returned 0" into "the thing
is actually up" before a cutover is called done.

## Declaring hooks

Hooks live in the source's checkout at `.exocomp/hooks.yml`, with an operator
override at `<data-dir>/hooks/<source>.yml` that wins when present — the same
override-wins precedence the source model uses everywhere.

```yaml
pre_run:
  - name: wait-db          # a shell step: an inline command...
    run: "pg_isready -h db"
  - name: seed             # ...or a script path, relative to the checkout
    script: scripts/seed.sh
    secret: pg             # optional: a run_secret binding, injected as named env

post_run:
  - name: warm-cache
    run: "curl -sf http://127.0.0.1:8080/warm"

gate:
  - name: web-up           # HTTP: 2xx by default, or a declared status
    http:
      url: http://127.0.0.1:8080/health
      expect_status: 200
  - name: cert-fresh       # TLS handshake + not-after floor (days)
    tls:
      address: example.internal:443
      min_days: 14
  - name: dns-cutover      # resolves, optionally to an expected value
    dns:
      name: app.example.internal
      expect: 10.0.0.42
  - name: lag-ok           # a numeric metric body compared to a threshold
    metric:
      url: http://127.0.0.1:9000/replication_lag_seconds
      op: "<"
      value: 30
```

A step names exactly one of `run` (an inline shell command) or `script` (a path
inside the checkout, run with `/bin/sh`). A gate probe names exactly one of
`http`, `dns`, `tls`, `metric`. Unknown keys, a step with neither `run` nor
`script`, and a probe that cannot decide are all **refused at define time** — a
gate that cannot decide would silently pass, which a safety gate must never do.

## Commands

```
exocomp hooks validate path/to/hooks.yml   # refuse a malformed file (exits non-zero)
exocomp hooks show <source>                 # print the resolved lifecycle
exocomp run <source> <playbook> --wait      # run the composed workflow
exocomp run status <workflow-id>            # read the ledger: which step, what state
```

`hooks validate` reads a file and reports the first problem; it is safe to run in
a repo's CI so a broken `hooks.yml` never reaches a source. `exocomp run` needs a
running `exocomp serve` — the daemon's sequencer is what drives the chain and
resumes it after a restart, exactly like the terraform→ansible handoff. The
playbook path is relative to the source checkout.

A source's lifecycle applies no matter which command runs the play, not just
`exocomp run`:

```
exocomp playbook run --source <source> <playbook> --wait   # a plain play, wrapped
exocomp handoff run --source <source> <workspace> <playbook>  # the apply->play chain, wrapped
```

`playbook run --source` composes the same `[pre_run] -> play -> [post_run] -> gate`
workflow; `handoff run --source` wraps its `apply -> play` chain as
`[pre_run] -> apply -> play -> [post_run] -> gate`. Both need a running daemon
when the source has hooks, for the same reason `run` does. A source with no
`hooks.yml` leaves either command behaving exactly as it did without `--source`.

`run status` shows each step and its state; a `blocked` step is the visible proof
that a failed predecessor retired the rest:

```
workflow run:app:... : failed
  step 1  hook          succeeded  job 4
  step 2  ansible       succeeded  job 5     # the play exited 0...
  step 3  verify        failed     job 6     # ...but the gate failed the run
```

The failing probe is named in the run's event stream (`gate FAILED: web-up`), so
"why did this fail" reads straight off the output.

## Boundaries worth knowing

- **Hooks run sandboxed, user-space.** Each hook is a shell step under the same
  nono tier as every other job: the checkout is granted read+write, the shell's
  tool tree read, and the data dir's database and keys denied regardless of what
  the hook does. Hooks run as the run user; nothing here runs sudo. A hook is
  local prep — it gets no network egress. The network is the gate's job.
- **The gate runs in the daemon, not a subprocess.** A probe is a control-plane
  read over a local or reachable endpoint, like drift collection — there is
  nothing to sandbox, and the daemon's redactor scrubs a probe's output. Each
  probe has a timeout; an endpoint that never answers is a failed probe, not a
  hang.
- **A hook's secret is a binding name, never a value.** `secret:` names a
  run_secret binding; the sealed values are resolved from the envelope at
  execution time and injected as named environment variables the hook inherits.
  They never touch the argv, the job row, or an event — the same credential plane
  a dynamic-inventory plugin or a provider authenticates from.
- **The TLS probe checks the not-after floor, not chain validity.** An internal
  step-ca or self-signed endpoint is a legitimate target; the handshake proves
  the endpoint serves TLS and `min_days` is the "warn me before the cert expires"
  ladder. Use a metric or HTTP probe when you need to assert chain trust.
- **A source with no `hooks.yml` pays nothing** — its run is just the play,
  unchanged.
