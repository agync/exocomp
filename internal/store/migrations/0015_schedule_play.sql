-- A recurring play over a STANDING inventory: kind 'play' names a registered
-- inventory (args.target) and a playbook (args.argv), so the `tf:<workspace>`
-- inventory an apply refreshes carries a newly provisioned host into the next
-- converge without the schedule being edited. SQLite cannot widen a CHECK in
-- place, so the table is rebuilt exactly as 0005 rebuilt it; nothing references
-- schedule, which is what makes that safe.
CREATE TABLE schedule_v3 (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL UNIQUE,
    cron_expr       TEXT    NOT NULL,
    kind            TEXT    NOT NULL CHECK (kind IN ('shell', 'backup', 'tofu-plan', 'play')),
    args_json       TEXT    NOT NULL DEFAULT '{}',
    concurrency_key TEXT    NOT NULL DEFAULT '',
    jitter_s        INTEGER NOT NULL DEFAULT 0,
    enabled         INTEGER NOT NULL DEFAULT 1,
    last_job_id     INTEGER REFERENCES job (id),
    next_at         INTEGER
);

INSERT INTO schedule_v3 (id, name, cron_expr, kind, args_json, concurrency_key,
                         jitter_s, enabled, last_job_id, next_at)
SELECT id, name, cron_expr, kind, args_json, concurrency_key,
       jitter_s, enabled, last_job_id, next_at
FROM schedule;

DROP TABLE schedule;

ALTER TABLE schedule_v3 RENAME TO schedule;

CREATE INDEX schedule_enabled ON schedule (enabled);
