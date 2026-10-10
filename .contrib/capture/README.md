# Capture rig

Regenerates the web UI renders under `docs/design/assets/web/` from a live
exocomp daemon, so a picture in the wiki is produced rather than hand-made.

    ./capture-renders.sh        # seed, verify, shoot
    ./render-all.sh             # annotate + promote into docs/design/assets/web/
    ./capture-ansible.sh        # + the managed-node captures (lima)
    ./capture-k3s.sh            # + the live k3s apply capture (lima)
    ./snapshot.sh save <name>   # cache the data dir for fast iteration
    ./snapshot.sh restore <name>

Requires `make build` first, plus a local `playwright-cli` reachable through
`npx --no-install`. The rig never installs anything.

## The eight renders, and which need a VM

| Render | Needs |
|---|---|
| `1a` to `1e`, `dashboard-dark`, `dashboard-light` | nothing |
| `1f` shared vocabulary | nothing, because it is not a capture |

None of the seven live renders needs a lima VM. Every one of them is SQLite
state: rows the CLI can create. The up and down split comes from checks aimed at
addresses that answer and addresses that cannot, so the probe subsystem produces
the states on its own. `1f` is a design sheet that shows every state chip at
once, which no single live surface does, so the rig does not produce it and it
stays as committed.

## Design decisions worth knowing

**A dedicated port, 18642.** The rig runs on a host that may already carry a live
`exocompd`, the operator's own on 8642 and 8643. Binding that port either fails
or, worse, succeeds and shadows it. The daemon helpers read their pidfile and
data dir from `EXOCOMP_E2E_DATA`, so every call sets it; without it they operate
on the e2e rig's daemon instead of this one.

**TLS by default.** A cleartext daemon stamps an "insecure HTTP" banner across
every page, which the originals do not carry. So the rig serves TLS and clicks
through Chrome's self-signed interstitial, because `playwright-cli` exposes no
ignore-https-errors flag. `EXOCOMP_CAPTURE_TLS=0` turns it off.

**Dark by media emulation.** The "system" theme resolves from
`prefers-color-scheme`, so the rig emulates the media feature. Pinning
`data-theme` on the document looks equivalent but a fresh page load drops it.

**Verify before shooting.** A render is only worth capturing if the state behind
it is the state the render claims, so the rig asserts the seeded state and fails
loudly. Probe results land on the sweep's own clock, so the wait polls the
assertion rather than sleeping a fixed time: a full sweep is about eleven
seconds, three ping targets at two seconds each plus a five second HTTP timeout.

**The deploy key counts as a secret.** `source add` seals a deploy key of its
own, so `secrets list` returns three rows for two secrets a person added. The
assertions check names, never counts.

## What the rig cannot reproduce

The probe subsystem runs with `AllowLoopback: false` as an SSRF guard, so a
loopback target can never read up. The original `1b` render shows `loopback` as
unobserved while `presence` and `web-health` already carry failure history; the
rig cannot hold that combination, because the reload that makes the daemon aware
of a new check sweeps every check, so `loopback` reads down like the rest.

The consequence is visible: `1b` reads "1 of 1 down" and "5 DOWN" on the
dashboard rather than "4 of 5 targets down" with "1 target never observed". The
state is honest, the counts differ from the archived render.

The daemon uptime in the shell header and the wall clock are also not
controllable without elevation.

## Snapshots are a cache, not an artifact

`snapshot.sh` tars the data dir for fast iteration when a CSS token changes and
reshooting eight renders is the only thing that moved. It must not be committed:
seeded timestamps drift toward meaninglessness, it depends on `token rotate`
invalidating secrets on restore, and a stale snapshot produces captures that
look subtly wrong, which is worse than a failure.
