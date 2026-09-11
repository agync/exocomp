# exocomp ansible e2e — project "infra"

Source checkout registered as `infra` in the throwaway daemon. The playbook
manages the lima node `node0` (a real Fedora VM) and writes a marker file that
is the independent observable side effect — a pass is a REAL pass (the marker
exists with the expected content), never exit-0-alone.

## Per-run wiring

The lima SSH endpoint (host/port/key) is re-derived EVERY run (the forwarded
port changes across VM restarts). `lib/prepare.sh` writes two files into this
checkout before the source is registered:

- `inventory.ini` — the ansible inventory: host `lima-exocomp-node0` (the
  name that matches the per-run ssh config stanza), vars point ansible at the
  per-run ssh config and identity key.
- `ssh_config` — the per-run `limactl show-ssh --format=config` output.

Both are gitignored (they are per-run secrets-ish paths). The playbook and
`ansible.cfg` are committed.

## The play

`plays/site.yml`:

- `check marker` — stat the marker file (always).
- `write marker` — write `marker.txt` to `/tmp/exocomp-e2e/` with the content
  from a play variable (`marker_content`), idempotent (`creates`).
- `install motd` — a visible host state (the motd) so the demo has a second
  observable fact.

The job is verified from the host: `limactl shell node0 -- cat
/tmp/exocomp-e2e/marker.txt` must equal the expected content.
