-- Cross-host single-active lease (the resilience fence). The host-local flock in
-- serve.lock stops two daemons on ONE host; it says nothing across hosts sharing
-- one replicated store (a Litestream/restored copy, a shared volume), where two
-- daemons could each go active and split-brain the queue. This row is that
-- missing cross-host guard: the active daemon holds it and renews renewed_at
-- within its wall-clock ttl_ms; a starting daemon that finds it LIVE (renewed
-- within ttl_ms) refuses to become active rather than double-activate.
--
-- Exactly one lease exists: id is pinned to 1, so a second acquirer contends for
-- the same row under the store's one writer instead of minting a second active
-- identity. A STALE lease (now past renewed_at + ttl_ms) may be taken; taking a
-- LIVE one held by someone else is the operator's explicit --force-lease call.
CREATE TABLE serve_lease (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    -- Who holds it — hostname and pid, human-legible because a refused daemon
    -- names it back to the operator ("held by <holder>"), never an opaque id.
    holder_id   TEXT    NOT NULL,
    -- unix milliseconds, UTC. acquired_at is when THIS holder first took the
    -- lease; renewed_at moves on every renew. Liveness is now < renewed_at + ttl.
    acquired_at INTEGER NOT NULL,
    renewed_at  INTEGER NOT NULL,
    -- The TTL the holder committed to renewing within, stored on the row so a
    -- reader on another host judges liveness without sharing the holder's flags.
    ttl_ms      INTEGER NOT NULL CHECK (ttl_ms > 0)
);
