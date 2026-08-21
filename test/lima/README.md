# test/lima

Rootless Fedora VM used as a dummy managed node for exocomp integration tests.

Run `./smoke.sh` to start the node (first run downloads Fedora), wait for
SSH, print `uname -a`, install `ansible-core` user-scope via `uv` if
missing, and assert an `ansible -m ping` pong.

Run `./smoke.sh stop` to stop the node, or `limactl delete exocomp-node0`
to delete it.
