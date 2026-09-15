-- Widen check_row.kind to admit the dns/tls/metric kinds the shared probe
-- vocabulary adds (plan C2/D2). SQLite cannot alter a CHECK in place, and
-- check_state holds an incoming FK REFERENCES check_row (ON DELETE CASCADE), so
-- both tables are rebuilt: check_state_v2 references check_row_v2, the old
-- child is dropped before the old parent (dropping the parent first would
-- cascade the child away), and the parent is renamed back, which repoints
-- check_state_v2's FK at the new check_row.
CREATE TABLE check_row_v2 (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL UNIQUE,
    kind          TEXT    NOT NULL CHECK (kind IN ('tcp', 'ping', 'http', 'dns', 'tls', 'metric')),
    interval_s    INTEGER NOT NULL,
    targets_json  TEXT    NOT NULL DEFAULT '[]',
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    snoozed_until INTEGER NOT NULL DEFAULT 0
);

INSERT INTO check_row_v2 (id, name, kind, interval_s, targets_json, enabled, created_at, updated_at, snoozed_until)
SELECT id, name, kind, interval_s, targets_json, enabled, created_at, updated_at, snoozed_until
FROM check_row;

CREATE TABLE check_state_v2 (
    check_id            INTEGER NOT NULL REFERENCES check_row_v2 (id) ON DELETE CASCADE,
    target_name         TEXT    NOT NULL,
    status              TEXT    NOT NULL CHECK (status IN ('up', 'down')),
    consecutive         INTEGER NOT NULL DEFAULT 0,
    emitted             TEXT    NOT NULL DEFAULT '' CHECK (emitted IN ('', 'up', 'down')),
    first_seen          INTEGER NOT NULL,
    last_change         INTEGER NOT NULL,
    recent_observations TEXT    NOT NULL DEFAULT '[]',
    netgroup            TEXT    NOT NULL DEFAULT '',
    last_duration_ms    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (check_id, target_name)
);

INSERT INTO check_state_v2 (check_id, target_name, status, consecutive, emitted, first_seen, last_change, recent_observations, netgroup, last_duration_ms)
SELECT check_id, target_name, status, consecutive, emitted, first_seen, last_change, recent_observations, netgroup, last_duration_ms
FROM check_state;

DROP TABLE check_state;
DROP TABLE check_row;

ALTER TABLE check_row_v2 RENAME TO check_row;
ALTER TABLE check_state_v2 RENAME TO check_state;
