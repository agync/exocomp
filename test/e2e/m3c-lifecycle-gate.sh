#!/bin/sh
# M3c lifecycle gate: pre/post-run hooks + the post-run health/verification gate,
# both as step-ledger steps. A run of a source composes into one workflow —
# [pre_run...] -> the play -> [post_run...] -> the gate — driven by one serve
# daemon. This gate proves:
#   1. a malformed .exocomp/hooks.yml is refused at validate time (a good one is
#      accepted, so the refusal is the semantics, not a parser that says no to all);
#   2. a passing gate (HTTP 2xx) lets the workflow SUCCEED;
#   3. a post_run HTTP gate FAILS a run whose play exited 0 but whose endpoint
#      returns 500 — the workflow is failed, the play step is still succeeded
#      (exit-0 main), and the gate names the failing probe;
#   4. a pre_run hook that fails leaves the play BLOCKED and never run (the passing
#      run above is the companion that proves the play otherwise DOES run);
#   5. a TLS not-after gate variant: a 2-day cert fails a 14-day floor and passes a
#      1-day floor — real handshake in the daemon.
# Real ansible + real nono sandbox through one serve daemon, like m3c-story5-gate.sh.
set -eu

ROOT=$(git rev-parse --show-toplevel)
WORK=$(mktemp -d)
SERVE_PID=""
OK_PID=""
BAD_PID=""
TLS_PID=""
cleanup() {
    for pid in "$SERVE_PID" "$OK_PID" "$BAD_PID" "$TLS_PID"; do
        [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
    done
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

log() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

export PATH="$HOME/.local/bin:$PATH"
export XDG_DATA_HOME="$WORK/data"
DATADIR="$XDG_DATA_HOME/exocomp"

command -v ansible-playbook >/dev/null 2>&1 || { log "ansible-playbook not installed; skipping"; exit 0; }
command -v python3 >/dev/null 2>&1 || { log "python3 not installed; skipping"; exit 0; }

# A tiny HTTP server that always answers with one status code, so the gate's
# HTTP probe has a stable 200 endpoint and a stable 500 endpoint to decide on.
http_server() {
    python3 - "$1" "$2" >/dev/null 2>&1 &
    printf '%s' "$!"
} <<'PY'
import sys, http.server
code = int(sys.argv[2])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(code); self.end_headers(); self.wfile.write(b"x")
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY

# A TLS listener serving one cert, so the gate's tls probe can handshake and read
# the leaf's not-after. It answers HTTP too, but the probe only needs the
# handshake.
tls_server() {
    python3 - "$1" "$2" "$3" >/dev/null 2>&1 &
    printf '%s' "$!"
} <<'PY'
import sys, ssl, http.server
port, cert, key = int(sys.argv[1]), sys.argv[2], sys.argv[3]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"x")
    def log_message(self, *a): pass
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
httpd = http.server.HTTPServer(("127.0.0.1", port), H)
httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
httpd.serve_forever()
PY

PLAY_MARKER="PLAY-RAN-MARKER-7c1f"

start_daemon() {
    "$EXO" serve --drain-grace 15s >"$1" 2>&1 &
    SERVE_PID=$!
    for _ in $(seq 1 60); do
        grep -q 'state backend listening' "$1" 2>/dev/null && break
        sleep 0.2
    done
    grep -q 'state backend listening' "$1" || { cat "$1"; fail "daemon did not start"; }
}

log "build + init"
make -C "$ROOT" -s build
EXO="$ROOT/bin/exocomp"
"$EXO" init >"$WORK/kit.txt"

# --- 1. malformed hooks.yml is refused at validate time ------------------------
log "a malformed hooks.yml is refused at validate time"
cat >"$WORK/bad-hooks.yml" <<'EOF'
pre_run:
  - name: broken
    typo: this-key-does-not-exist
EOF
if "$EXO" hooks validate "$WORK/bad-hooks.yml" >"$WORK/validate-bad.out" 2>&1; then
    cat "$WORK/validate-bad.out"; fail "a malformed hooks.yml was accepted"
fi
grep -qi 'field typo not found\|typo' "$WORK/validate-bad.out" \
    || { cat "$WORK/validate-bad.out"; fail "validate did not explain the malformed key"; }

# Companion: a well-formed file IS accepted, so the refusal is the semantics.
cat >"$WORK/good-hooks.yml" <<'EOF'
pre_run:
  - name: prep
    run: "true"
gate:
  - name: web-up
    http:
      url: http://127.0.0.1:1/
EOF
"$EXO" hooks validate "$WORK/good-hooks.yml" >"$WORK/validate-good.out" 2>&1 \
    || { cat "$WORK/validate-good.out"; fail "a well-formed hooks.yml was refused"; }
grep -q ' ok ' "$WORK/validate-good.out" || { cat "$WORK/validate-good.out"; fail "validate did not confirm the good file"; }

# --- the source: a volpath tree OUTSIDE the data dir (sandbox refuses an ancestor) ---
SRC="$WORK/src"
mkdir -p "$SRC/.exocomp"
cat >"$SRC/site.yml" <<EOF
- hosts: all
  gather_facts: false
  connection: local
  tasks:
    - ansible.builtin.debug:
        msg: $PLAY_MARKER
EOF
"$EXO" source add app --path "$SRC" --kind volpath >/dev/null

log "start health servers (200 and 500) and the daemon"
OK_PID=$(http_server 18081 200)
BAD_PID=$(http_server 18082 500)
start_daemon "$WORK/serve.log"
# Wait for the health endpoints to answer before the gate probes them.
for _ in $(seq 1 50); do
    python3 -c 'import urllib.request,sys; urllib.request.urlopen("http://127.0.0.1:18081/",timeout=1)' 2>/dev/null && break
    sleep 0.1
done

run_app() {
    "$EXO" run app site.yml --wait >"$1" 2>&1 || true
    sed -n 's/^workflow \(run:[^ ]*\) started.*/\1/p' "$1" | head -1
}

# --- 2. a passing gate lets the workflow succeed, and the play runs ------------
log "a pre_run hook + a passing HTTP gate -> workflow SUCCEEDS"
cat >"$SRC/.exocomp/hooks.yml" <<'EOF'
pre_run:
  - name: prep
    run: "true"
gate:
  - name: web-up
    http:
      url: http://127.0.0.1:18081/
      expect_status: 200
EOF
WF=$(run_app "$WORK/pass.out")
[ -n "$WF" ] || { cat "$WORK/pass.out"; fail "no workflow id (pass case)"; }
"$EXO" run status "$WF" >"$WORK/pass.status" 2>&1
grep -q "workflow $WF: succeeded" "$WORK/pass.status" || { cat "$WORK/pass.status"; fail "passing run not succeeded"; }
grep -Eq 'step 1  hook +succeeded' "$WORK/pass.status" || { cat "$WORK/pass.status"; fail "pre-hook not succeeded"; }
grep -Eq 'step 2  ansible +succeeded' "$WORK/pass.status" || { cat "$WORK/pass.status"; fail "play not succeeded"; }
grep -Eq 'step 3  verify +succeeded' "$WORK/pass.status" || { cat "$WORK/pass.status"; fail "gate not succeeded"; }
# The play actually ran (its marker printed): the block test below removes this.
grep -q "$PLAY_MARKER" "$WORK/pass.out" || { cat "$WORK/pass.out"; fail "the play did not run in the passing case"; }
grep -q 'gate passed' "$WORK/pass.out" || { cat "$WORK/pass.out"; fail "gate did not report passing"; }

# --- 3. a post_run HTTP gate fails an exit-0 run, naming the probe -------------
log "an HTTP gate returning 500 FAILS a run whose play exited 0"
cat >"$SRC/.exocomp/hooks.yml" <<'EOF'
pre_run:
  - name: prep
    run: "true"
gate:
  - name: web-up
    http:
      url: http://127.0.0.1:18082/
      expect_status: 200
EOF
WF=$(run_app "$WORK/gatefail.out")
[ -n "$WF" ] || { cat "$WORK/gatefail.out"; fail "no workflow id (gate-fail case)"; }
"$EXO" run status "$WF" >"$WORK/gatefail.status" 2>&1
grep -q "workflow $WF: failed" "$WORK/gatefail.status" || { cat "$WORK/gatefail.status"; fail "gate-fail workflow not failed"; }
# The crux: the play step SUCCEEDED (exit 0) while the workflow FAILED on the gate.
grep -Eq 'step 2  ansible +succeeded' "$WORK/gatefail.status" || { cat "$WORK/gatefail.status"; fail "play step not succeeded (exit-0 main)"; }
grep -Eq 'step 3  verify +failed' "$WORK/gatefail.status" || { cat "$WORK/gatefail.status"; fail "gate step not failed"; }
grep -q "$PLAY_MARKER" "$WORK/gatefail.out" || { cat "$WORK/gatefail.out"; fail "the play did not run (exit-0 main expected)"; }
grep -q 'gate FAILED: web-up' "$WORK/gatefail.out" || { cat "$WORK/gatefail.out"; fail "the gate did not name the failing probe"; }

# --- 4. a failing pre_run hook leaves the play blocked and never run -----------
log "a pre_run hook that fails leaves the play BLOCKED and never run"
cat >"$SRC/.exocomp/hooks.yml" <<'EOF'
pre_run:
  - name: prep
    run: "exit 1"
gate:
  - name: web-up
    http:
      url: http://127.0.0.1:18081/
      expect_status: 200
EOF
WF=$(run_app "$WORK/block.out")
[ -n "$WF" ] || { cat "$WORK/block.out"; fail "no workflow id (block case)"; }
"$EXO" run status "$WF" >"$WORK/block.status" 2>&1
grep -q "workflow $WF: failed" "$WORK/block.status" || { cat "$WORK/block.status"; fail "block workflow not failed"; }
grep -Eq 'step 1  hook +failed' "$WORK/block.status" || { cat "$WORK/block.status"; fail "failing pre-hook not failed"; }
grep -Eq 'step 2  ansible +blocked' "$WORK/block.status" || { cat "$WORK/block.status"; fail "play not blocked behind the failed hook"; }
grep -Eq 'step 3  verify +blocked' "$WORK/block.status" || { cat "$WORK/block.status"; fail "gate not blocked behind the failed hook"; }
# The play must NOT have run: its marker is absent (the passing case proved it prints).
grep -q "$PLAY_MARKER" "$WORK/block.out" && fail "the play ran despite the pre-hook failing" || true

# --- 5. a TLS not-after gate variant ------------------------------------------
if command -v openssl >/dev/null 2>&1; then
    log "a TLS not-after gate: a 2-day cert fails a 14-day floor, passes a 1-day floor"
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
        -keyout "$WORK/key.pem" -out "$WORK/cert.pem" -days 2 -nodes \
        -subj "/CN=probe.test" >/dev/null 2>&1 || fail "openssl could not mint a cert"
    TLS_PID=$(tls_server 18443 "$WORK/cert.pem" "$WORK/key.pem")
    for _ in $(seq 1 50); do
        python3 -c 'import ssl,socket; ctx=ssl._create_unverified_context(); s=ctx.wrap_socket(socket.create_connection(("127.0.0.1",18443),timeout=1),server_hostname="probe.test"); s.close()' 2>/dev/null && break
        sleep 0.1
    done

    cat >"$SRC/.exocomp/hooks.yml" <<'EOF'
gate:
  - name: cert-fresh
    tls:
      address: 127.0.0.1:18443
      min_days: 14
EOF
    WF=$(run_app "$WORK/tlsfail.out")
    "$EXO" run status "$WF" >"$WORK/tlsfail.status" 2>&1
    grep -q "workflow $WF: failed" "$WORK/tlsfail.status" || { cat "$WORK/tlsfail.out"; fail "a 2-day cert did not fail a 14-day floor"; }
    grep -q 'gate FAILED: cert-fresh' "$WORK/tlsfail.out" || { cat "$WORK/tlsfail.out"; fail "the TLS gate did not name cert-fresh"; }

    cat >"$SRC/.exocomp/hooks.yml" <<'EOF'
gate:
  - name: cert-fresh
    tls:
      address: 127.0.0.1:18443
      min_days: 1
EOF
    WF=$(run_app "$WORK/tlspass.out")
    "$EXO" run status "$WF" >"$WORK/tlspass.status" 2>&1
    grep -q "workflow $WF: succeeded" "$WORK/tlspass.status" || { cat "$WORK/tlspass.out"; fail "a 2-day cert did not pass a 1-day floor"; }
else
    log "openssl not installed; skipping the TLS not-after variant"
fi

log "PASS: hooks compose as ledger steps; the health gate fails exit-0 runs, blocks on a failed pre-hook, and passes when healthy"
