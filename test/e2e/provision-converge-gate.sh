#!/bin/sh
# Provision -> onboard -> converge gate (v1.1 items A and B).
#
# A: one resumable four-step ledger chain. A workspace's declared outputs name a
# host nobody has reached yet; the chain applies, WAITS (bounded) for that
# host's SSH to come up, runs the seeded onboarding runbook against it (pinning
# the host key first, then pushing the fleet key and creating the automation
# user), and finally runs the configure play — all against the inventory the
# apply materialized. Step N+1 runs only if step N succeeded, and a daemon
# killed mid-chain resumes it from the ledger.
#
# B: `tf:<workspace>` is a STANDING inventory. A `schedule --kind play` targets
# it by name, so a host a later apply adds is converged on the next tick without
# the schedule being touched — and a hostless inventory fails closed.
#
# The "provisioned" host is the lima node (test/lima). Its sshd is reached
# through a TCP proxy this script starts LATE and on purpose: that is what makes
# the wait step wait for something real rather than for an already-open port.
set -eu

ROOT=$(git rev-parse --show-toplevel)
WORK=$(mktemp -d)
INSTANCE=${EXOCOMP_LIMA_INSTANCE:-exocomp-node0}
SERVE_PID=""
PROXY_PID=""
cleanup() {
    [ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null || true
    [ -n "$PROXY_PID" ] && kill "$PROXY_PID" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

export PATH="$HOME/.local/bin:$PATH"
export XDG_DATA_HOME="$WORK/data"
DATADIR="$XDG_DATA_HOME/exocomp"

for tool in tofu ansible-playbook ansible-galaxy python3 ssh-keyscan limactl; do
    command -v "$tool" >/dev/null 2>&1 || { echo "SKIP: $tool absent"; exit 0; }
done
state=$(limactl list --format '{{.Status}}' "$INSTANCE" 2>/dev/null || true)
[ "$state" = "Running" ] || { echo "SKIP: lima instance $INSTANCE not running (test/lima/smoke.sh)"; exit 0; }

LIMA_PORT=$(limactl list --format '{{.SSHLocalPort}}' "$INSTANCE")
LIMA_USER=$(awk '/^  User /{print $2; exit}' "$HOME/.lima/$INSTANCE/ssh.config")
LIMA_KEY=$(awk '/IdentityFile/{gsub(/"/,"",$2); print $2; exit}' "$HOME/.lima/$INSTANCE/ssh.config")
[ -n "$LIMA_PORT" ] && [ -n "$LIMA_USER" ] && [ -n "$LIMA_KEY" ] || fail "could not read $INSTANCE ssh facts"
log "lima node $INSTANCE: sshd on 127.0.0.1:$LIMA_PORT as $LIMA_USER"

# The proxy port stands in for "the address the provisioned host will answer
# on". Nothing listens there yet, which is the point.
PROXY_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')

cat >"$WORK/proxy.py" <<'EOF'
# Minimal TCP forwarder: 127.0.0.1:<listen> -> 127.0.0.1:<target>. It stands in
# for the moment a freshly provisioned machine's sshd starts accepting.
import socket, sys, threading

listen_port, target_port = int(sys.argv[1]), int(sys.argv[2])


def pipe(src, dst):
    try:
        while True:
            chunk = src.recv(65536)
            if not chunk:
                break
            dst.sendall(chunk)
    except OSError:
        pass
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except OSError:
            pass


def serve(client):
    try:
        upstream = socket.create_connection(("127.0.0.1", target_port))
    except OSError:
        client.close()
        return
    threading.Thread(target=pipe, args=(client, upstream), daemon=True).start()
    threading.Thread(target=pipe, args=(upstream, client), daemon=True).start()


srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", listen_port))
srv.listen(32)
while True:
    conn, _ = srv.accept()
    threading.Thread(target=serve, args=(conn,), daemon=True).start()
EOF

start_daemon() {
    # The sandbox tier grants ansible egress on port 22 only and never grants a
    # path outside the data dir, so the lima rig — a forwarded high port and the
    # operator's own key — needs the subprocess-boundary tier. The sandbox is
    # proven by its own gate; this one proves the chain.
    "$EXO" serve --sandbox local --drain-grace 15s --reload-interval 2s >"$1" 2>&1 &
    SERVE_PID=$!
    for _ in $(seq 1 100); do
        grep -q 'scheduler started' "$1" 2>/dev/null && break
        sleep 0.2
    done
    grep -q 'scheduler started' "$1" || { cat "$1"; fail "daemon did not start"; }
}

steps_of() { "$EXO" provision status "$1" 2>&1; }

log "build + init"
make -C "$ROOT" -s build
EXO="$ROOT/bin/exocomp"
"$EXO" init >"$WORK/kit.txt"

log "vendor ansible.posix into the offline cache (the onboarding runbook needs it)"
cat >"$WORK/requirements.yml" <<'EOF'
collections:
  - name: ansible.posix
EOF

# --- the workspace whose outputs name the not-yet-reachable host ---
mkdir -p "$WORK/tf" "$WORK/play"
write_tf() {
    cat >"$WORK/tf/main.tf" <<EOF
resource "local_file" "marker" {
  filename = "\${path.module}/marker.txt"
  content  = "provisioned"
}

locals {
  conn = {
    ansible_host                 = "127.0.0.1"
    ansible_port                 = $PROXY_PORT
    ansible_user                 = "$LIMA_USER"
    ansible_ssh_private_key_file = "$LIMA_KEY"
    ansible_ssh_common_args      = "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
    ansible_python_interpreter   = "/usr/bin/python3"
  }
}

output "db_password" {
  value     = "$SENSITIVE"
  sensitive = true
}

output "ansible_inventory" {
  value = {
    all = {
      children = {
        provisioned = {
          hosts = {
$1
          }
        }
      }
    }
  }
}
EOF
}
SENSITIVE="provision-CANARY-4d19"
write_tf '            node0 = local.conn'
"$EXO" workspace add edge --dir "$WORK/tf" >/dev/null

# The configure play: a real change on the node, one marker per inventory host,
# so "which hosts did this converge reach" is answerable from the node itself.
cat >"$WORK/play/converge.yml" <<'EOF'
- name: Converge the provisioned hosts
  hosts: all
  gather_facts: false
  tasks:
    - name: Leave a per-host marker
      ansible.builtin.file:
        path: "/tmp/exocomp-converge-{{ inventory_hostname }}"
        state: touch
        mode: "0644"
EOF

log "start the daemon"
start_daemon "$WORK/serve.log"

"$EXO" playbook vendor --requirements "$WORK/requirements.yml" >"$WORK/vendor.out" 2>&1 \
    || { cat "$WORK/vendor.out"; fail "vendoring ansible.posix failed"; }

# ---------------------------------------------------------------------------
# A. the four-step chain, with the wait waiting for a port that is not open yet
# ---------------------------------------------------------------------------
log "start the chain: apply -> wait for SSH -> onboard -> converge"
( "$EXO" provision run edge "$WORK/play/converge.yml" --ssh-wait 180s --wait \
    >"$WORK/provision.out" 2>&1; echo "$?" >"$WORK/provision.rc" ) &

for _ in $(seq 1 200); do
    [ -s "$WORK/provision.out" ] && grep -q '^workflow provision:' "$WORK/provision.out" && break
    sleep 0.2
done
WF=$(sed -n 's/^workflow \(provision:[^ ]*\) started.*/\1/p' "$WORK/provision.out" | head -1)
[ -n "$WF" ] || { cat "$WORK/provision.out"; fail "no workflow id"; }
log "workflow $WF"

log "the chain reaches the wait step and BLOCKS there: nothing listens on $PROXY_PORT yet"
for _ in $(seq 1 300); do
    steps_of "$WF" >"$WORK/status-wait.out" 2>&1 || true
    grep -Eq 'step 2  ssh-wait +running' "$WORK/status-wait.out" && break
    sleep 0.3
done
grep -Eq 'step 1  tofu-apply +succeeded' "$WORK/status-wait.out" \
    || { cat "$WORK/status-wait.out"; fail "the apply did not succeed"; }
grep -Eq 'step 2  ssh-wait +running' "$WORK/status-wait.out" \
    || { cat "$WORK/status-wait.out"; fail "the chain never reached the wait step"; }
grep -Eq 'step 3  host-onboard +pending' "$WORK/status-wait.out" \
    || { cat "$WORK/status-wait.out"; fail "the onboard step ran before the host was reachable"; }

# Prove the wait is really blocked on the port rather than racing it: hold here
# a few seconds with the port still closed, and check the step is STILL running.
sleep 5
steps_of "$WF" >"$WORK/status-wait2.out" 2>&1
grep -Eq 'step 2  ssh-wait +running' "$WORK/status-wait2.out" \
    || { cat "$WORK/status-wait2.out"; fail "the wait step did not wait for the closed port"; }
log "the wait step is still waiting after 5s with the port closed — it waits for real"

log "bring the host up: start the sshd proxy on $PROXY_PORT"
python3 "$WORK/proxy.py" "$PROXY_PORT" "$LIMA_PORT" >"$WORK/proxy.log" 2>&1 &
PROXY_PID=$!

log "the chain finishes: wait clears, the host is onboarded, the play converges it"
for _ in $(seq 1 400); do
    [ -f "$WORK/provision.rc" ] && break
    sleep 0.5
done
[ -f "$WORK/provision.rc" ] || { cat "$WORK/provision.out"; fail "the chain did not finish"; }
[ "$(cat "$WORK/provision.rc")" = "0" ] || { cat "$WORK/provision.out"; fail "the chain failed"; }

steps_of "$WF" >"$WORK/status.out" 2>&1
grep -q "workflow $WF: succeeded" "$WORK/status.out" || { cat "$WORK/status.out"; fail "workflow not succeeded"; }
for want in 'step 1  tofu-apply' 'step 2  ssh-wait' 'step 3  host-onboard' 'step 4  ansible-play'; do
    grep -Eq "$want +succeeded" "$WORK/status.out" \
        || { cat "$WORK/status.out"; fail "$want did not succeed"; }
done
log "four ledger steps, all succeeded:"
cat "$WORK/status.out"

log "the apply materialized an inventory naming the derived host"
INV="$DATADIR/artifacts/inventory/edge.json"
[ -f "$INV" ] || fail "no materialized inventory at $INV"
grep -q node0 "$INV" || { cat "$INV"; fail "the inventory does not name the host"; }
grep -q "$SENSITIVE" "$INV" && fail "the sensitive output leaked into the inventory" || true
grep -q 'naming 1 host' "$WORK/provision.out" \
    || { cat "$WORK/provision.out"; fail "the capture did not report the host count"; }

log "the host key was pinned BEFORE the onboarding pushed anything"
grep -q 'pinned .* host key(s) for node0 before onboarding it' "$WORK/provision.out" \
    || { cat "$WORK/provision.out"; fail "the chain did not pin the host key first"; }
"$EXO" host known-hosts >"$WORK/known.out" 2>&1
grep -q "\[127.0.0.1\]:$PROXY_PORT" "$WORK/known.out" \
    || { cat "$WORK/known.out"; fail "the derived host's key is not in the known_hosts store"; }

log "the node itself carries the automation user and the fleet key"
FLEET=$(sed -n 's/^\(ssh-ed25519 [A-Za-z0-9+/=]* exocomp-fleet\)$/\1/p' "$WORK/provision.out" | head -1)
[ -n "$FLEET" ] || { cat "$WORK/provision.out"; fail "the chain printed no fleet public key"; }
limactl shell "$INSTANCE" -- id ansible >"$WORK/id.out" 2>&1 \
    || { cat "$WORK/id.out"; fail "the onboarding did not create the automation user"; }
limactl shell "$INSTANCE" -- sudo cat /home/ansible/.ssh/authorized_keys >"$WORK/authkeys.out" 2>&1 \
    || { cat "$WORK/authkeys.out"; fail "the automation user has no authorized_keys"; }
grep -qF "$FLEET" "$WORK/authkeys.out" \
    || { cat "$WORK/authkeys.out"; fail "the fleet key was not authorized on the node"; }

log "the configure play ran against the tf-derived inventory, on the node"
limactl shell "$INSTANCE" -- test -f /tmp/exocomp-converge-node0 \
    || fail "the configure play left no marker on the provisioned host"

# ---------------------------------------------------------------------------
# the wait is BOUNDED: a host that never comes up fails the chain, not hangs it
# ---------------------------------------------------------------------------
log "a host that never answers fails the wait step and blocks the rest"
DEAD_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')
mkdir -p "$WORK/tfdead"
sed "s/= $PROXY_PORT\$/= $DEAD_PORT/" "$WORK/tf/main.tf" >"$WORK/tfdead/main.tf"
"$EXO" workspace add dead --dir "$WORK/tfdead" >/dev/null
"$EXO" provision run dead "$WORK/play/converge.yml" --ssh-wait 5s --wait \
    >"$WORK/dead.out" 2>&1 || true
WFD=$(sed -n 's/^workflow \(provision:[^ ]*\) started.*/\1/p' "$WORK/dead.out" | head -1)
[ -n "$WFD" ] || { cat "$WORK/dead.out"; fail "no dead-host workflow id"; }
steps_of "$WFD" >"$WORK/dead-status.out" 2>&1
grep -Eq 'step 2  ssh-wait +failed' "$WORK/dead-status.out" \
    || { cat "$WORK/dead-status.out"; fail "the wait step did not fail on an unreachable host"; }
grep -Eq 'step 3  host-onboard +blocked' "$WORK/dead-status.out" \
    || { cat "$WORK/dead-status.out"; fail "the onboard step was not blocked"; }
grep -Eq 'step 4  ansible-play +blocked' "$WORK/dead-status.out" \
    || { cat "$WORK/dead-status.out"; fail "the configure play was not blocked"; }
log "bounded wait proved: unreachable host -> step 2 failed, steps 3-4 blocked"

# ---------------------------------------------------------------------------
# a daemon killed mid-chain resumes from the ledger
# ---------------------------------------------------------------------------
log "a daemon killed mid-chain resumes the chain from the ledger"
GATE="$WORK/converge-gate"
cat >"$WORK/play/gated.yml" <<EOF
- name: Converge, once the gate opens
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait for the gate
      ansible.builtin.shell: "i=0; while [ ! -f $GATE ] && [ \$i -lt 900 ]; do sleep 0.2; i=\$((i+1)); done"
      delegate_to: localhost
      changed_when: false
    - name: Leave the resume marker
      ansible.builtin.file:
        path: "/tmp/exocomp-resumed-{{ inventory_hostname }}"
        state: touch
        mode: "0644"
EOF
limactl shell "$INSTANCE" -- rm -f /tmp/exocomp-resumed-node0 >/dev/null 2>&1 || true

"$EXO" provision run edge "$WORK/play/gated.yml" --ssh-wait 180s >"$WORK/resume.out" 2>&1 \
    || { cat "$WORK/resume.out"; fail "the resume chain did not start"; }
WFR=$(sed -n 's/^workflow \(provision:[^ ]*\) started.*/\1/p' "$WORK/resume.out" | head -1)
[ -n "$WFR" ] || { cat "$WORK/resume.out"; fail "no resume workflow id"; }

for _ in $(seq 1 400); do
    steps_of "$WFR" >"$WORK/resume-status.out" 2>&1 || true
    grep -Eq 'step 3  host-onboard +succeeded' "$WORK/resume-status.out" \
        && grep -Eq 'step 4  ansible-play +running' "$WORK/resume-status.out" && break
    sleep 0.3
done
grep -Eq 'step 3  host-onboard +succeeded' "$WORK/resume-status.out" \
    || { cat "$WORK/resume-status.out"; fail "the chain did not reach the configure play"; }

log "kill -9 the daemon with the configure play in flight"
kill -9 "$SERVE_PID" 2>/dev/null || true
wait "$SERVE_PID" 2>/dev/null || true
SERVE_PID=""
: >"$GATE"

log "restart the daemon; it resumes the chain from the ledger"
start_daemon "$WORK/serve2.log"
for _ in $(seq 1 400); do
    steps_of "$WFR" >"$WORK/resume-status2.out" 2>&1 || true
    grep -q "workflow $WFR: succeeded" "$WORK/resume-status2.out" && break
    grep -q "workflow $WFR: failed" "$WORK/resume-status2.out" && { cat "$WORK/resume-status2.out"; fail "the resumed chain failed"; }
    sleep 0.5
done
grep -q "workflow $WFR: succeeded" "$WORK/resume-status2.out" \
    || { cat "$WORK/resume-status2.out"; fail "the chain did not resume to succeeded"; }
grep -q 'resumed .* workflow' "$WORK/serve2.log" \
    || { cat "$WORK/serve2.log"; fail "the daemon did not report resuming from the ledger"; }
limactl shell "$INSTANCE" -- test -f /tmp/exocomp-resumed-node0 \
    || fail "the resumed configure play never reached the host"
# The steps that had already succeeded were not run again.
grep -Eq 'step 1  tofu-apply +succeeded' "$WORK/resume-status2.out" \
    || { cat "$WORK/resume-status2.out"; fail "the apply step is not succeeded after the resume"; }
log "resume proved: the chain continued at the interrupted step"

# ---------------------------------------------------------------------------
# B. the standing inventory a schedule targets by name
# ---------------------------------------------------------------------------
log "a SCHEDULED play targets the standing tf:edge inventory"
limactl shell "$INSTANCE" -- rm -f /tmp/exocomp-converge-node0 /tmp/exocomp-converge-node1 >/dev/null 2>&1 || true
"$EXO" schedule add fleet-converge --cron '@every 10s' --kind play --target tf:edge \
    --concurrency-key fleet-converge -- "$WORK/play/converge.yml" >"$WORK/sched.out" 2>&1 \
    || { cat "$WORK/sched.out"; fail "schedule add --kind play failed"; }
grep -q 'schedule fleet-converge (play) added' "$WORK/sched.out" \
    || { cat "$WORK/sched.out"; fail "the play schedule was not registered"; }

for _ in $(seq 1 120); do
    limactl shell "$INSTANCE" -- test -f /tmp/exocomp-converge-node0 >/dev/null 2>&1 && break
    sleep 1
done
limactl shell "$INSTANCE" -- test -f /tmp/exocomp-converge-node0 \
    || { cat "$WORK/serve2.log"; fail "the scheduled play never converged the standing inventory's host"; }
log "the scheduled play converged node0 from tf:edge"

log "a host a later apply adds joins the recurring converge on the next tick"
write_tf '            node0 = local.conn
            node1 = local.conn'
"$EXO" workspace apply edge >"$WORK/apply2.out" 2>&1 \
    || { cat "$WORK/apply2.out"; fail "the refreshing apply failed"; }
grep -q 'naming 2 host' "$WORK/apply2.out" \
    || { cat "$WORK/apply2.out"; fail "the apply did not refresh the inventory to 2 hosts"; }
for _ in $(seq 1 120); do
    limactl shell "$INSTANCE" -- test -f /tmp/exocomp-converge-node1 >/dev/null 2>&1 && break
    sleep 1
done
limactl shell "$INSTANCE" -- test -f /tmp/exocomp-converge-node1 \
    || { cat "$WORK/serve2.log"; fail "the new host was not converged by the next tick"; }
log "node1 appeared in tf:edge on apply and was converged without touching the schedule"
"$EXO" schedule rm fleet-converge >/dev/null

log "a hostless standing inventory fails closed: the schedule enqueues nothing"
mkdir -p "$WORK/tfempty"
cat >"$WORK/tfempty/main.tf" <<'EOF'
output "region" {
  value = "eu-north"
}
EOF
"$EXO" workspace add hostless --dir "$WORK/tfempty" >/dev/null
"$EXO" workspace apply hostless >"$WORK/hostless.out" 2>&1 \
    || { cat "$WORK/hostless.out"; fail "the hostless apply failed"; }
grep -q 'naming 0 host' "$WORK/hostless.out" \
    || { cat "$WORK/hostless.out"; fail "the hostless apply reported hosts"; }
jobcount() {
    if command -v sqlite3 >/dev/null 2>&1; then
        sqlite3 "$DATADIR/exocomp.db" "SELECT count(*) FROM job;"
    else
        echo skip
    fi
}
JOBS_BEFORE=$(jobcount)
"$EXO" schedule add empty-converge --cron '@every 5s' --kind play --target tf:hostless \
    -- "$WORK/play/converge.yml" >/dev/null
sleep 12
grep -q 'names no hosts' "$WORK/serve2.log" \
    || { tail -30 "$WORK/serve2.log"; fail "the scheduler did not refuse the hostless inventory"; }
JOBS_AFTER=$(jobcount)
[ "$JOBS_BEFORE" = "$JOBS_AFTER" ] \
    || fail "the hostless schedule enqueued jobs ($JOBS_BEFORE -> $JOBS_AFTER)"
log "fail-closed proved: a hostless tf:hostless inventory ran no play at all"

log "ALL PROVISION->CONVERGE GATE CHECKS PASSED"
