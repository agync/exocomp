#!/bin/sh
# Face drivers for the tofu e2e rig: one function per surface that a scenario
# can call to drive the same verb through the CLI, the web UI, or the MCP
# endpoint, plus the token env they share. Sourced by run-e2e.sh.

# --- CLI leg (local mode against the daemon's data dir) ----------------------

# cli_add <name> <dir>  — register a tofu workspace
cli_add() {
    XDG_DATA_HOME="$SCRATCH" "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" workspace add "$1" --dir "$2"
}

# cli_plan <name> — returns 0/1/2 like tofu (2 = changes present)
cli_plan() {
    XDG_DATA_HOME="$SCRATCH" "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" workspace plan "$1" >/dev/null 2>&1
    return $?
}

# cli_apply <name> [--plan <artifact>] [--run-secrets <set>]
cli_apply() {
    XDG_DATA_HOME="$SCRATCH" "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" workspace apply "$@"
}

# cli_list — print workspace rows (id name dir serial)
cli_list() {
    XDG_DATA_HOME="$SCRATCH" "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" workspace list
}

# cli_unlock <name>
cli_unlock() {
    XDG_DATA_HOME="$SCRATCH" "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" workspace unlock "$1"
}

# cli_prune <name> [--keep N] [--days N]
cli_prune() {
    XDG_DATA_HOME="$SCRATCH" "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" workspace prune "$@"
}

# cli_secret_add <name> — seal a value read from stdin (never echoed)
cli_secret_add() {
    XDG_DATA_HOME="$SCRATCH" "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" secrets add "$1"
}

# cli_run_secret_add <set> <env> <secret>
cli_run_secret_add() {
    XDG_DATA_HOME="$SCRATCH" "${EXOCOMP_BIN:-$ROOT/bin/exocomp}" run-secret add "$1" "$2" "$3"
}

# --- Web leg ----------------------------------------------------------------
# Uses the playwright-cli skill's helpers when present; otherwise the scenario
# skips with a note. Screenshots land under $SHOT_DIR/<scenario>/.
# The web surface needs the operator token as a bearer cookie; the login flow
# is exercised by POSTing the token once.

# web_shot <scenario> <name> — screenshot the current page
web_shot() {
    mkdir -p "$SHOT_DIR/$1"
    # playwright-cli provides `pw` wrappers; if unavailable, log a skip.
    if command -v pwshot >/dev/null 2>&1; then
        pwshot "$SHOT_DIR/$1/$2.png"
    else
        log "  (web screenshot $1/$2 skipped: pwshot not on PATH)"
    fi
}

# --- MCP leg ----------------------------------------------------------------
# Direct JSON-RPC over POST /mcp (docs/operations/mcp.md). No stdio wrapper.

MCP_URL="http://${EXOCOMP_E2E_ADDR:-0.0.0.0:8642}/mcp"

# mcp_call <method> <params-json> — one tools/call, prints the result envelope
mcp_call() {
    method=$1
    params=$2
    curl -s -X POST "$MCP_URL" \
        -H "Authorization: Bearer ${EXOCOMP_OPERATOR_TOKEN:-sandbox-ui}" \
        -H "Content-Type: application/json" \
        -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$method\",\"arguments\":$params}}"
}

# mcp_list — print the tool catalog as one line per id
mcp_list() {
    curl -s -X POST "$MCP_URL" \
        -H "Authorization: Bearer ${EXOCOMP_OPERATOR_TOKEN:-sandbox-ui}" \
        -H "Content-Type: application/json" \
        -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
}
