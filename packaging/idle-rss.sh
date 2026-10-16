#!/bin/sh
# Measure the daemon's idle resident set: start `serve` against a scratch data
# dir, sample /proc/<pid>/status until the numbers stop moving, stop it, and
# print the peak in kB. The budget this feeds is in
# docs/sbom/bill-of-materials.md; `make budgets` turns it into a pass or fail.
set -eu

bin=${1:?usage: idle-rss.sh <path to exocomp binary>}
samples=${2:-10}

scratch=$(mktemp -d)
cleanup() {
	[ -n "${pid:-}" ] && kill -TERM "$pid" 2>/dev/null || true
	rm -rf "$scratch"
}
trap cleanup EXIT

XDG_DATA_HOME=$scratch "$bin" init >"$scratch/init.log" 2>&1 || {
	echo "idle-rss: init failed" >&2
	cat "$scratch/init.log" >&2
	exit 1
}

# The sandbox tier does not affect the daemon's idle footprint, and a bare
# runner has no `nono` on PATH, so run it opportunistically: use the sandbox
# when present, degrade loudly when not, and measure the same baseline either
# way.
XDG_DATA_HOME=$scratch "$bin" serve --port 0 --sandbox opportunistic >"$scratch/serve.log" 2>&1 &
pid=$!

# The queue line is the last thing startup prints before the daemon is idle.
waited=0
until grep -q "queue draining" "$scratch/serve.log" 2>/dev/null; do
	kill -0 "$pid" 2>/dev/null || {
		echo "idle-rss: serve exited during startup" >&2
		cat "$scratch/serve.log" >&2
		exit 1
	}
	sleep 0.2
	waited=$((waited + 1))
	[ "$waited" -lt 100 ] || { echo "idle-rss: serve never became ready" >&2; exit 1; }
done

peak=0
i=0
while [ "$i" -lt "$samples" ]; do
	rss=$(awk '/^VmRSS:/ {print $2}' "/proc/$pid/status" 2>/dev/null || echo 0)
	[ "$rss" -gt "$peak" ] && peak=$rss
	sleep 0.5
	i=$((i + 1))
done

echo "$peak"
