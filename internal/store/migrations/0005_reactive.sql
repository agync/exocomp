-- The reactive layer's ledgers. Timestamps are INTEGER unix milliseconds (UTC).
--
-- Every received event gets a row whatever happens to it, including the ones
-- nothing matched: "why didn't my rule fire?" has to be a query. trace_json
-- holds the per-condition evaluation trace as it was decided, because the
-- ledgers it read (silences, fires, caps) move afterwards and a recomputed
-- trace would answer a different question than the one asked.

CREATE TABLE rule (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    name             TEXT    NOT NULL UNIQUE,
    source_filter    TEXT    NOT NULL DEFAULT '',
    cel_expr         TEXT    NOT NULL,
    action_kind      TEXT    NOT NULL,
    action_args_json TEXT    NOT NULL DEFAULT '{}',
    cooldown_s       INTEGER NOT NULL DEFAULT 0,
    daily_cap        INTEGER NOT NULL DEFAULT 0,
    require_approval INTEGER NOT NULL DEFAULT 0,
    enabled          INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX rule_enabled ON rule (enabled);

CREATE TABLE event (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    source          TEXT    NOT NULL,
    received_at     INTEGER NOT NULL,
    dedup_key       TEXT    NOT NULL DEFAULT '',
    normalized_json TEXT    NOT NULL DEFAULT '{}',
    raw_json        TEXT    NOT NULL DEFAULT '{}',
    matched_rule    TEXT    NOT NULL DEFAULT '',
    outcome         TEXT    NOT NULL CHECK (outcome IN (
                        'ignored', 'matched', 'suppressed', 'gated', 'fired', 'error')),
    reason          TEXT    NOT NULL DEFAULT '',
    job_id          INTEGER REFERENCES job (id) ON DELETE SET NULL,
    trace_json      TEXT    NOT NULL DEFAULT '[]'
);

CREATE INDEX event_received_at ON event (received_at, id);
CREATE INDEX event_dedup_key ON event (dedup_key, received_at) WHERE dedup_key <> '';

-- The cooldown and daily-cap ledgers are these rows, not counters in memory:
-- a crash-loop restart must not hand a rule a fresh budget.
CREATE TABLE rule_fire (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id INTEGER NOT NULL REFERENCES rule (id) ON DELETE CASCADE,
    ts      INTEGER NOT NULL,
    job_id  INTEGER REFERENCES job (id) ON DELETE SET NULL
);

CREATE INDEX rule_fire_rule_ts ON rule_fire (rule_id, ts);

-- expires_at is NOT NULL by design: a silence that never lifts is how a
-- homelab stops noticing it is broken.
CREATE TABLE silence (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scope_kind  TEXT    NOT NULL CHECK (scope_kind IN ('alert', 'host', 'global')),
    scope_value TEXT    NOT NULL DEFAULT '',
    reason      TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    created_via TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX silence_expires_at ON silence (expires_at);

-- token_hash is sha256 of the capability handed out; the token itself is
-- never stored, so a stolen database cannot approve anything. workspace and
-- plan_serial are what the stale-plan refusal compares against: a plan whose
-- workspace moved after it was made is refused, not silently applied.
CREATE TABLE approval (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id        INTEGER NOT NULL UNIQUE REFERENCES job (id) ON DELETE CASCADE,
    plan_artifact INTEGER REFERENCES artifact (id) ON DELETE SET NULL,
    workspace     TEXT    NOT NULL DEFAULT '',
    plan_serial   INTEGER NOT NULL DEFAULT 0,
    requested_at  INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    token_hash    TEXT    NOT NULL,
    state         TEXT    NOT NULL CHECK (state IN (
                      'pending', 'approved', 'rejected', 'expired', 'withdrawn')),
    decided_at    INTEGER,
    decided_via   TEXT    NOT NULL DEFAULT '',
    reason        TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX approval_state ON approval (state, expires_at);

-- Drift detection is a scheduled `tofu plan`: exit code 2 becomes an event in
-- the same pipeline, so an apply can be gated behind the same approval as any
-- other reactive action. SQLite cannot widen a CHECK in place, so the table is
-- rebuilt; nothing references schedule, which is what makes that safe here.
CREATE TABLE schedule_v2 (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL UNIQUE,
    cron_expr       TEXT    NOT NULL,
    kind            TEXT    NOT NULL CHECK (kind IN ('shell', 'backup', 'tofu-plan')),
    args_json       TEXT    NOT NULL DEFAULT '{}',
    concurrency_key TEXT    NOT NULL DEFAULT '',
    jitter_s        INTEGER NOT NULL DEFAULT 0,
    enabled         INTEGER NOT NULL DEFAULT 1,
    last_job_id     INTEGER REFERENCES job (id),
    next_at         INTEGER
);

INSERT INTO schedule_v2 (id, name, cron_expr, kind, args_json, concurrency_key,
                         jitter_s, enabled, last_job_id, next_at)
SELECT id, name, cron_expr, kind, args_json, concurrency_key,
       jitter_s, enabled, last_job_id, next_at
FROM schedule;

DROP TABLE schedule;

ALTER TABLE schedule_v2 RENAME TO schedule;

CREATE INDEX schedule_enabled ON schedule (enabled);
