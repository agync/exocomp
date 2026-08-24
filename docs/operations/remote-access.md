# Remote CLI: driving a daemon over the tailnet

`exocomp login` pins a remote daemon's certificate and a bearer token so the same CLI can
drive a box other than the one it runs on — the remote surface specified in
[the agent-interface design](../design/agent-interface.md), ratified by
[ADR 0014](../adr/0014-agent-facing-surface.md). It covers reads and a growing set of
writes; see [Which verbs are remoted](#which-verbs-are-remoted-today) below.

## Pinning a remote (TOFU)

```
exocomp login server.tailnet.ts.net:8443 --token exo_...
```

`login` connects over TLS, captures the daemon's self-signed certificate fingerprint (the
same SHA-256 hex `doctor` and the web login screen print), shows it, and — on confirmation
— records `{endpoint, fingerprint, token}` under an alias in
`~/.config/exocomp/hosts.toml` (`0600`, atomic write via temp file + rename), the same
trust-on-first-use discipline as `~/.ssh/known_hosts`. `--name` sets the alias explicitly;
otherwise it is derived from the endpoint's first host label (`server.tailnet.ts.net` →
`server`). The first login becomes the default remote unless `--no-default` is passed or a
default is already set.

**A changed fingerprint is refused, loudly.** If the alias is already pinned to a
*different* fingerprint, `login` prints both values, warns that this is either an
out-of-band re-key or a machine-in-the-middle, and requires `--assume-yes` to overwrite —
exactly the discipline a changed SSH host key demands. A first-time pin still asks for
interactive confirmation (`y/N`) unless `--assume-yes` is given; a non-interactive session
with no `--assume-yes` refuses rather than blocking on stdin.

A raw HTTP endpoint is rejected everywhere in the client (`must be https`): a bearer token
over cleartext is sniffable, so there is no insecure remote path to opt into.

## Selecting a remote: flags, env, precedence

Every command inherits two global flags plus the token flag:

| Input | Precedence |
|---|---|
| `--remote <selector>` | wins over the environment |
| `EXOCOMP_REMOTE` | used when `--remote` is not passed |
| neither | the command stays **local**, against the data dir, unchanged |
| `--remote=` (empty value) | remote mode is active; resolves to the `hosts.toml` default |

`<selector>` is either a stored alias (`server`) or a raw `https://` endpoint that was
pinned under some alias by an earlier `login` — an endpoint that was never logged in is
refused (TOFU demands the pin exist first). The `hosts.toml` default is consulted **only**
once remote mode is already active (via `--remote`/`EXOCOMP_REMOTE`); a bare command with
neither never gets silently redirected off the box, even when a default remote is stored.

The bearer token resolves the same way `login` resolves it: `--token` flag → `EXOCOMP_TOKEN`
→ the token `login` stored for that alias. `--token`/`EXOCOMP_TOKEN` let one invocation use
a different-scoped token than the one pinned, without a re-login.

```
exocomp --remote server jobs list --output json
EXOCOMP_REMOTE=server EXOCOMP_TOKEN=exo_... exocomp doctor
exocomp --remote https://server.tailnet.ts.net:8443 workspace list
```

`--remote` is deliberately not `--host`: `--host` already names the ansible host *pattern*
on `command`/`ansible` (which managed nodes a play targets) — a different axis from which
daemon the CLI itself talks to. Reusing it would conflate the two.

## Which verbs are remoted today

The client (`internal/client`) implements the `/api/v1` read and write surface (see
[api.md](api.md)). In client mode these verbs re-render the daemon's `wire` bytes exactly —
`--output json` in remote mode is byte-identical to `--output json` run locally against the
same resource:

| Verb | Remoted |
|---|---|
| `exocomp doctor` | yes — shows the **remote** daemon's health summary, not this workstation's |
| `exocomp jobs list` / `get` / `events` / `follow` | yes |
| `exocomp approval list` / `decide` | yes |
| `exocomp source list`, `workspace list` | yes |
| `exocomp workspace plan` / `apply` | yes — enqueues on the daemon and follows the job |
| `exocomp playbook run` | yes — enqueues on the daemon and follows the job |
| `exocomp drain` | yes — cordons the remote daemon ([drain mode](drain-mode.md)) |

Still local-only: every `source`/`inventory`/`runsecret`/`schedule`/`secrets` write,
`runner add`, and `backup run` run against the local data dir; there is no `--remote` path
for them yet. See [Driving a remote daemon](remote-cli.md) for the write flow — what each
write returns, idempotency, and the inputs that do not cross the wire.

A remote failure surfaces the daemon's `wire` error envelope (preserving its taxonomy
`code`, not just a status) on `--output json`, or the plain error on text; a certificate
that no longer matches the pin is a `PinMismatchError`, refused before any request is sent,
telling the operator to re-run `login` if the re-key is expected.

Related: [Machine-readable output](machine-output.md) · [The API surface](api.md) ·
[LAN access](lan-access.md).
