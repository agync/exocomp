#!/bin/sh
# Shared helpers for the capture rig. Every stage driver sources this.
#
# The rig reuses the tofu-e2e throwaway daemon rather than owning one: the only
# difference is the data dir, so the token minting, the TLS switch and the
# --keep-data snapshot behaviour all come for free.
set -eu

# The caller sets HERE to its own directory before sourcing; when common.sh is
# executed directly $0 is lib/, so both resolve to the repo root.
HERE=${HERE:-$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)}
ROOT=$(CDPATH= cd -- "$HERE/../.." && pwd)
CAP="$ROOT/.contrib/capture"
DAEMON="$ROOT/.contrib/tofu-e2e/lib/daemon.sh"

# An isolated data dir so capture state never collides with the e2e rig.
SCRATCH="${EXOCOMP_CAPTURE_DATA:-/tmp/exocomp-throwaway/capture}"
SHOTS="$SCRATCH/shots"
OUT="${EXOCOMP_CAPTURE_OUT:-$ROOT/docs/design/assets/web}"
BROWSER_W=1600
BROWSER_H=1000

# A dedicated port. The rig runs on a host that may already carry a live
# exocompd (the operator's own, on 8642/8643), and binding that port would
# either fail or, worse, succeed and shadow it. 18642 is out of the way.
CAPTURE_ADDR="${EXOCOMP_CAPTURE_ADDR:-127.0.0.1:18642}"
# TLS by default, so the captures carry the fingerprint login page rather than
# the cleartext warning banner the originals do not have.
CAPTURE_TLS="${EXOCOMP_CAPTURE_TLS:-1}"
if [ "$CAPTURE_TLS" = "1" ]; then
    export EXOCOMP_CAPTURE_BASE="https://$CAPTURE_ADDR"
else
    export EXOCOMP_CAPTURE_BASE="http://$CAPTURE_ADDR"
fi

log() { printf 'capture: %s\n' "$*" >&2; }
die() { log "FAIL: $*"; exit 1; }

# Seed steps go through the CLI against the daemon's own data dir, the same
# local mode the e2e rig uses. Local mode writes the store directly, so the
# running daemon has to be told to pick the new rows up before a capture that
# depends on them (a check has to be probed, a schedule has to be scheduled).
reload_daemon() {
    . "$SCRATCH/tokens.env"
    # -k: the rig's daemon carries a self-signed cert and is local and
    # throwaway. Without it every reload reads 000 and the state looks stale
    # for no reason a reader can see.
    code=$(curl -sk -o /dev/null -w '%{http_code}' -X POST \
        -H "Authorization: Bearer $EXOCOMP_OPERATOR_TOKEN" \
        "$EXOCOMP_CAPTURE_BASE/api/v1/maintenance/reload" || true)
    [ "$code" = "200" ] || log "warn: reload returned $code (state may be stale)"
}

# The daemon helpers resolve their pidfile and data dir from EXOCOMP_E2E_DATA,
# so every call must set it. Without it they default to the e2e rig's dir and
# operate on the wrong daemon, which is how a stale pidfile survives a wipe.
capture_daemon_stop() {
    EXOCOMP_E2E_DATA="$SCRATCH" "$DAEMON" stop >/dev/null 2>&1 || true
}

capture_daemon_start() {
    keep=${1:-}
    tls=""
    [ "$CAPTURE_TLS" = "1" ] && tls=1
    EXOCOMP_E2E_DATA="$SCRATCH" EXOCOMP_E2E_ADDR="$CAPTURE_ADDR" \
        EXOCOMP_E2E_TLS="${tls:-0}" "$DAEMON" start $keep >/dev/null 2>&1
    [ -f "$SCRATCH/tokens.env" ] || die "daemon produced no tokens.env"
    . "$SCRATCH/tokens.env"
}

# Seed steps go through the CLI against the daemon's own data dir, the same
# local mode the e2e rig uses.
cli() { XDG_DATA_HOME="$SCRATCH" "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" "$@"; }

# playwright-cli is a local npx package, not a global command; --no-install
# refuses to reach the network so a missing package fails loudly instead of
# silently downloading one. The npx update banner goes to stderr on every call
# and would drown the rig's own log, so it is dropped here.
PW() { npx --no-install playwright-cli "$@" 2>/dev/null; }

browser_ok() { npx --no-install playwright-cli --version >/dev/null 2>&1; }

# The web surface resolves the "system" theme from prefers-color-scheme, and the
# captures are dark because dark is the native theme. Emulating the media
# feature is deterministic and survives navigation, unlike pinning data-theme on
# the document, which a fresh page load drops.
force_dark_theme() { PW run-code "async page => { await page.emulateMedia({ colorScheme: 'dark' }); }" >/dev/null; }

# Open the browser, log in through the real form, and park the session cookie
# the same way a paste login does.
#
# TLS mode is the default because the renders this rig reproduces were captured
# over TLS: a cleartext daemon stamps an "insecure HTTP" banner across every
# page, which the originals do not carry. A self-signed cert therefore has to be
# accepted, and Chrome's interstitial is the only route, since playwright-cli
# exposes no ignore-https-errors flag. The click-through targets roles and text,
# not refs, because refs are per-session.
browser_login() {
    base=$1
    # A self-signed cert makes `open` exit non-zero with an error page loaded,
    # which is the expected start of the click-through, so the failure is
    # tolerated here and the goto below is what asserts we really landed.
    PW open "$base/login" >/dev/null 2>&1 || true
    if [ "${EXOCOMP_CAPTURE_TLS:-1}" = "1" ]; then
        PW click 'button:has-text("Advanced")' >/dev/null 2>&1 || true
        PW click 'text=/Proceed to/' >/dev/null 2>&1 || true
        PW goto "$base/login" >/dev/null || die "could not reach $base/login after accepting the cert"
    fi
    PW resize "$BROWSER_W" "$BROWSER_H" >/dev/null
    PW fill 'input[name="token"]' "$EXOCOMP_OPERATOR_TOKEN" >/dev/null
    PW click 'form button[type="submit"], form input[type="submit"]' >/dev/null
    # The login POST redirects to the dashboard; wait for it to settle so the
    # first goto does not race the redirect.
    PW goto "$base/" >/dev/null
}

# shot <route> <out-name>: navigate to a route and capture it.
shot() {
    route=$1
    name=$2
    mkdir -p "$SHOTS"
    PW goto "$EXOCOMP_CAPTURE_BASE$route" >/dev/null || die "goto $route failed"
    PW screenshot --filename="$SHOTS/$name.png" >/dev/null || die "screenshot $name failed"
    log "  shot $name.png ($route)"
}

# annotate <in.png> <id> <title> <subtitle>: stamp the design-review band the
# originals carry, so a render still reads as a labelled artboard rather than a
# bare screenshot. Cosmetically close, not pixel-identical.
annotate() {
    in=$1; id=$2; title=$3; sub=$4
    out=$5
    convert "$in" \
        -gravity north -background '#0A0C0D' -splice 0x64 \
        -fill '#8A9296' -font 'Adwaita-Mono' -pointsize 13 \
        -annotate +24+22 "$id" \
        -fill '#ECEFF0' -pointsize 15 -annotate +64+22 "$title" \
        -fill '#6B7477' -annotate +64+40 "$sub" \
        "$out" 2>/dev/null || die "annotate $name failed"
}

# wait_for_probe <seconds>: the up/down split in the checks renders is real, so
# the probe subsystem has to actually run a cycle before the capture.
wait_for_probe() {
    secs=${1:-20}
    log "  letting probes run ${secs}s so up/down states settle"
    i=0
    while [ "$i" -lt "$secs" ]; do
        sleep 5
        i=$((i + 5))
        printf '  ... %ss\n' "$i" >&2
    done
}
