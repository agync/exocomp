#!/bin/sh
# State seeding for the capture rig. Every function is idempotent and driven
# through the CLI, so the state a capture shows is declared here and nothing is
# built by hand in a browser.
set -eu

# seed_renders: the state the eight restored renders show. Deliberately the
# inverse of the renders: read one, create the rows it names.
seed_renders() {
    log "seeding render state"

    # 1b/1a — three checks, and the interesting one is about what does NOT get
    # probed. The probe subsystem runs with AllowLoopback: false (an SSRF guard),
    # so a loopback target can never read up; the original render shows loopback
    # as unobserved, which a long interval reproduces honestly: the first probe
    # is an hour away, so it reads "never seen yet" at capture time.
    cli check add loopback --kind tcp --interval 1h \
        --target local=127.0.0.1:22 >/dev/null
    # presence and web-health target addresses that cannot answer. Their targets
    # settle DOWN on their own, with no managed node anywhere.
    cli check add presence --kind ping --interval 5s \
        --target phone=10.255.255.1 \
        --target laptop=10.255.255.2 \
        --target tablet=10.255.255.3 >/dev/null
    cli check add web-health --kind http --interval 5s \
        --target vault=http://10.255.255.4/health >/dev/null

    # 1c/1d — one git source that has never synced. example.invalid is reserved
    # and never resolves, so "never synced" is the steady state rather than a
    # failure the rig has to provoke.
    cli source add site --url https://example.invalid/site.git --ref main >/dev/null

    # 1e — two secrets, one bound and spent, one unbound.
    printf 'not-a-real-restic-password\n' | cli secrets add restic-repo >/dev/null
    printf 'not-a-real-ntfy-token\n' | cli secrets add ntfy-token >/dev/null
}

# seed_backup_runs <count>: the usage metadata on restic-repo is spent by real
# backup runs, so make real ones. A local restic repo in the scratch dir keeps
# it off the host.
seed_backup_runs() {
    n=${1:-12}
    repo="$SCRATCH/restic-repo"
    mkdir -p "$repo" "$SCRATCH/payload"
    printf 'capture payload\n' >"$SCRATCH/payload/data.txt"
    export RESTIC_PASSWORD=not-a-real-restic-password
    i=0
    while [ "$i" -lt "$n" ]; do
        restic -r "$repo" backup "$SCRATCH/payload" >/dev/null 2>&1 ||
            die "restic backup run $((i + 1)) failed"
        i=$((i + 1))
    done
    unset RESTIC_PASSWORD
    log "  made $n backup runs into $repo"
}
