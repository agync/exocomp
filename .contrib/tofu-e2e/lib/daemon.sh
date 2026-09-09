#!/bin/sh
# Throwaway exocomp daemon for the tofu e2e rig. Runs bin/exocomp serve with
# the web+API surface on 0.0.0.0:8642 (insecure HTTP — loud, throwaway only)
# against an isolated XDG_DATA_HOME, and mints an operator and a readonly
# token. Usage:
#   lib/daemon.sh start [--keep-data]
#   lib/daemon.sh stop
#   lib/daemon.sh status
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
BIN="${EXOCOMP_BIN:-$ROOT/bin/exocomp}"
ADDR="${EXOCOMP_E2E_ADDR:-0.0.0.0:8642}"
BASE_DATA="${EXOCOMP_E2E_DATA:-/tmp/exocomp-throwaway/tofu-e2e}"
PIDFILE="$BASE_DATA/daemon.pid"
TOKENS="$BASE_DATA/tokens.env"
OPERATOR_TOKEN="sandbox-ui"
READONLY_TOKEN="sandbox-ui-readonly"
HOSTPORT=${ADDR##*:}
HOST=${ADDR%:*}

log() { printf 'daemon: %s\n' "$*" >&2; }

if [ ! -x "$BIN" ]; then
    log "exocomp binary not found at $BIN (run 'make build' from the repo root)"
    exit 1
fi

wait_listener() {
    i=0
    until (exec 3<>"/dev/tcp/${HOST:-127.0.0.1}/$HOSTPORT") 2>/dev/null; do
        i=$((i + 1))
        [ "$i" -lt 60 ] || { log "timed out waiting for $ADDR"; return 1; }
        sleep 1
    done
    exec 3>&- 2>/dev/null || true
    return 0
}

case "${1:-}" in
    start)
        if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
            log "already running (pid $(cat "$PIDFILE"))"
            exit 0
        fi
        if [ "${2:-}" != "--keep-data" ]; then
            rm -rf "$BASE_DATA"
        fi
        mkdir -p "$BASE_DATA"
        if [ ! -f "$BASE_DATA/exocomp/host.key" ]; then
            log "initializing data dir"
            XDG_DATA_HOME="$BASE_DATA" "$BIN" init >/dev/null 2>&1 || {
                log "init failed:"
                XDG_DATA_HOME="$BASE_DATA" "$BIN" init 2>&1 || true
                exit 1
            }
            log "minting operator + readonly tokens (pre-serve: the web listener refuses to bind without one)"
            # The token secret is one-time and printed by `token new`; capture it.
            OPERATOR_SECRET=$(XDG_DATA_HOME="$BASE_DATA" "$BIN" token new "$OPERATOR_TOKEN" --scope operator |
                grep -o 'exo_[0-9][0-9]*_[A-Za-z0-9_-]*' | head -1)
            READONLY_SECRET=$(XDG_DATA_HOME="$BASE_DATA" "$BIN" token new "$READONLY_TOKEN" --scope readonly |
                grep -o 'exo_[0-9][0-9]*_[A-Za-z0-9_-]*' | head -1)
            : "${OPERATOR_SECRET:?token mint produced no secret}"
            : "${READONLY_SECRET:?token mint produced no secret}"
        else
            log "data dir already initialized; recovering token secrets from the store"
            # Recover the one-time secrets: re-issue via rotate (the old secret
            # stops authenticating — fine for a throwaway).
            OPERATOR_SECRET=$(XDG_DATA_HOME="$BASE_DATA" "$BIN" token rotate "$OPERATOR_TOKEN" 2>/dev/null |
                grep -o 'exo_[0-9][0-9]*_[A-Za-z0-9_-]*' | head -1)
            READONLY_SECRET=$(XDG_DATA_HOME="$BASE_DATA" "$BIN" token rotate "$READONLY_TOKEN" 2>/dev/null |
                grep -o 'exo_[0-9][0-9]*_[A-Za-z0-9_-]*' | head -1)
        fi
        log "starting serve on $ADDR, data dir $BASE_DATA"
        # EXOCOMP_E2E_TLS=1 serves real TLS (self-signed, ADR 0009) instead of
        # cleartext --web-insecure-http. Used by the browser demo so the login
        # page shows the certificate fingerprint, not the "not secure" alert;
        # the matrix default stays insecure-http for its http:// curl and MCP
        # legs. Behind a tailscale serve https+insecure:// proxy the browser
        # sees a valid tailnet cert end to end.
        EXTRA_WEB="--web-insecure-http"
        if [ "${EXOCOMP_E2E_TLS:-0}" = "1" ]; then
            EXTRA_WEB=""
        fi
        # shellcheck disable=SC2086
        XDG_DATA_HOME="$BASE_DATA" "$BIN" serve --web-addr "$ADDR" $EXTRA_WEB \
            >"$BASE_DATA/daemon.log" 2>&1 &
        echo $! >"$PIDFILE"
        wait_listener || { log "serve did not come up; see $BASE_DATA/daemon.log"; exit 1; }
        sleep 1
        {
            echo "export EXOCOMP_E2E_ADDR=$ADDR"
            echo "export EXOCOMP_E2E_DATA=$BASE_DATA"
            echo "export EXOCOMP_OPERATOR_TOKEN=$OPERATOR_SECRET"
            echo "export EXOCOMP_READONLY_TOKEN=$READONLY_SECRET"
        } >"$TOKENS"
        log "ready. source $TOKENS for the token env"
        ;;
    stop)
        if [ -f "$PIDFILE" ]; then
            pid=$(cat "$PIDFILE")
            kill "$pid" 2>/dev/null || true
            rm -f "$PIDFILE"
            log "stopped pid $pid"
        else
            log "not running"
        fi
        ;;
    status)
        if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
            log "running (pid $(cat "$PIDFILE")) on $ADDR"
            exit 0
        fi
        log "not running"
        exit 1
        ;;
    *)
        echo "usage: $0 {start|stop|status} [--keep-data]" >&2
        exit 2
        ;;
esac
