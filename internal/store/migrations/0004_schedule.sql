-- Timestamps are INTEGER unix milliseconds (UTC); NULL means "not yet".
-- kind decides how a fire's args_json is interpreted: shell carries argv,
-- backup carries an optional target name (empty means every enabled target).

CREATE TABLE schedule (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL UNIQUE,
    cron_expr       TEXT    NOT NULL,
    kind            TEXT    NOT NULL CHECK (kind IN ('shell', 'backup')),
    args_json       TEXT    NOT NULL DEFAULT '{}',
    concurrency_key TEXT    NOT NULL DEFAULT '',
    jitter_s        INTEGER NOT NULL DEFAULT 0,
    enabled         INTEGER NOT NULL DEFAULT 1,
    last_job_id     INTEGER REFERENCES job (id),
    next_at         INTEGER
);

CREATE INDEX schedule_enabled ON schedule (enabled);
