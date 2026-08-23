-- Widen the schedule kind CHECK to admit 'drill': a recurring restore drill
-- (internal/backup.DrillJobKind). SQLite cannot widen a CHECK in place, so
-- the table is rebuilt exactly as 0015 rebuilt it; nothing references
-- schedule, which is what makes that safe.
CREATE TABLE schedule_v4 (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL UNIQUE,
    cron_expr       TEXT    NOT NULL,
    kind            TEXT    NOT NULL CHECK (kind IN ('shell', 'backup', 'tofu-plan', 'play', 'drill')),
    args_json       TEXT    NOT NULL DEFAULT '{}',
    concurrency_key TEXT    NOT NULL DEFAULT '',
    jitter_s        INTEGER NOT NULL DEFAULT 0,
    enabled         INTEGER NOT NULL DEFAULT 1,
    last_job_id     INTEGER REFERENCES job (id),
    next_at         INTEGER
);

INSERT INTO schedule_v4 (id, name, cron_expr, kind, args_json, concurrency_key,
                         jitter_s, enabled, last_job_id, next_at)
SELECT id, name, cron_expr, kind, args_json, concurrency_key,
       jitter_s, enabled, last_job_id, next_at
FROM schedule;

DROP TABLE schedule;

ALTER TABLE schedule_v4 RENAME TO schedule;

CREATE INDEX schedule_enabled ON schedule (enabled);
