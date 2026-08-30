-- Checks are named probe-sets: a kind, an interval, and a per-target address or
-- URL (targets_json, validated at write time in Go). check_state carries the
-- per-target transition state the sweep reads and writes. Refinement vs the
-- design doc §4.2: there is no `unknown` status — a missing row IS the
-- unobserved cold-start state; `consecutive` is the hysteresis counter and
-- `emitted` the last status that was emitted as an event. The '' value in the
-- emitted CHECK stays for schema compatibility but no live row carries it: the
-- sweep seeds emitted = result so a daemon restart never spurious-fires.
CREATE TABLE check_row (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    kind         TEXT    NOT NULL CHECK (kind IN ('tcp', 'ping', 'http')),
    interval_s   INTEGER NOT NULL,
    targets_json TEXT    NOT NULL DEFAULT '[]',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE TABLE check_state (
    check_id     INTEGER NOT NULL REFERENCES check_row (id) ON DELETE CASCADE,
    target_name  TEXT    NOT NULL,
    status       TEXT    NOT NULL CHECK (status IN ('up', 'down')),
    consecutive  INTEGER NOT NULL DEFAULT 0,
    emitted      TEXT    NOT NULL DEFAULT '' CHECK (emitted IN ('', 'up', 'down')),
    first_seen   INTEGER NOT NULL,
    last_change  INTEGER NOT NULL,
    PRIMARY KEY (check_id, target_name)
);