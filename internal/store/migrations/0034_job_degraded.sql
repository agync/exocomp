-- Widen the job state CHECK to admit 'degraded': a tolerated non-failure an
-- unreachable-tolerant ansible run settles into (the reachable hosts converged,
-- the offline ones are recorded and named, not paged).
--
-- SQLite cannot widen a CHECK in place, so job is rebuilt. Nine tables
-- reference job (job_event, artifact, tfstate, tflock, schedule, event,
-- rule_fire, approval, workflow_step), and a plain DROP of job performs an
-- implicit DELETE that cascades them all away — foreign_keys cannot be turned
-- off inside the migration transaction to stop it. So job is renamed aside
-- (which repoints every referencing FK to the renamed table), the new job is
-- built and repopulated, and every referencing table is rebuilt to point at the
-- new job before the old one is dropped, exactly as 0027 rebuilt inventory when
-- it rebuilt source. defer_foreign_keys holds enforcement to commit, when every
-- reference again resolves.
--
-- Ordering matters inside the rebuild: approval references artifact, so
-- artifact's legacy table is kept alive until approval has been rebuilt against
-- the new artifact — dropping it earlier would SET NULL every approval's
-- plan_artifact, losing the link.
PRAGMA defer_foreign_keys = ON;

ALTER TABLE job RENAME TO job_legacy;

CREATE TABLE job (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    runbook         TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    "trigger"       TEXT    NOT NULL,
    state           TEXT    NOT NULL CHECK (state IN (
                        'queued', 'awaiting_approval', 'running', 'succeeded',
                        'failed', 'cancelled', 'timed_out', 'interrupted', 'degraded')),
    args_json       TEXT    NOT NULL DEFAULT '{}',
    queued_at       INTEGER NOT NULL,
    started_at      INTEGER,
    finished_at     INTEGER,
    exit_code       INTEGER,
    concurrency_key TEXT    NOT NULL DEFAULT '',
    dedup_key       TEXT,
    idempotency_key TEXT    UNIQUE,
    owner_pid       INTEGER NOT NULL DEFAULT 0,
    partial         INTEGER NOT NULL DEFAULT 0
);

INSERT INTO job (id, runbook, kind, "trigger", state, args_json, queued_at,
                 started_at, finished_at, exit_code, concurrency_key, dedup_key,
                 idempotency_key, owner_pid, partial)
SELECT id, runbook, kind, "trigger", state, args_json, queued_at,
       started_at, finished_at, exit_code, concurrency_key, dedup_key,
       idempotency_key, owner_pid, partial
FROM job_legacy;

ALTER TABLE job_event RENAME TO job_event_legacy;

CREATE TABLE job_event (
    job_id   INTEGER NOT NULL REFERENCES job (id) ON DELETE CASCADE,
    seq      INTEGER NOT NULL,
    ts       INTEGER NOT NULL,
    level    TEXT    NOT NULL,
    msg      TEXT    NOT NULL,
    raw_json TEXT,
    PRIMARY KEY (job_id, seq)
);

INSERT INTO job_event (job_id, seq, ts, level, msg, raw_json)
SELECT job_id, seq, ts, level, msg, raw_json
FROM job_event_legacy;

DROP TABLE job_event_legacy;

CREATE TRIGGER job_event_append_only
BEFORE UPDATE ON job_event
BEGIN
    SELECT RAISE(ABORT, 'job_event is append-only');
END;

CREATE TRIGGER job_event_no_delete
BEFORE DELETE ON job_event
BEGIN
    SELECT RAISE(ABORT, 'job_event is append-only');
END;

ALTER TABLE tfstate RENAME TO tfstate_legacy;

CREATE TABLE tfstate (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id      INTEGER NOT NULL REFERENCES workspace (id) ON DELETE CASCADE,
    serial            INTEGER NOT NULL,
    lineage           TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    created_by_job_id INTEGER REFERENCES job (id) ON DELETE SET NULL,
    ciphertext        BLOB    NOT NULL
);

INSERT INTO tfstate (id, workspace_id, serial, lineage, created_at, created_by_job_id, ciphertext)
SELECT id, workspace_id, serial, lineage, created_at, created_by_job_id, ciphertext
FROM tfstate_legacy;

DROP TABLE tfstate_legacy;

CREATE UNIQUE INDEX tfstate_workspace_serial ON tfstate (workspace_id, serial);

ALTER TABLE tflock RENAME TO tflock_legacy;

CREATE TABLE tflock (
    workspace_id INTEGER PRIMARY KEY REFERENCES workspace (id) ON DELETE CASCADE,
    lock_id      TEXT    NOT NULL,
    who          TEXT    NOT NULL DEFAULT '',
    operation    TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    job_id       INTEGER REFERENCES job (id) ON DELETE SET NULL
);

INSERT INTO tflock (workspace_id, lock_id, who, operation, created_at, job_id)
SELECT workspace_id, lock_id, who, operation, created_at, job_id
FROM tflock_legacy;

DROP TABLE tflock_legacy;

ALTER TABLE schedule RENAME TO schedule_legacy;

CREATE TABLE schedule (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL UNIQUE,
    cron_expr       TEXT    NOT NULL,
    kind            TEXT    NOT NULL CHECK (kind IN ('shell', 'backup', 'tofu-plan', 'play', 'drill')),
    args_json       TEXT    NOT NULL DEFAULT '{}',
    concurrency_key TEXT    NOT NULL DEFAULT '',
    jitter_s        INTEGER NOT NULL DEFAULT 0,
    enabled         INTEGER NOT NULL DEFAULT 1,
    last_job_id     INTEGER REFERENCES job (id),
    next_at         INTEGER,
    last_run        INTEGER
);

INSERT INTO schedule (id, name, cron_expr, kind, args_json, concurrency_key,
                      jitter_s, enabled, last_job_id, next_at, last_run)
SELECT id, name, cron_expr, kind, args_json, concurrency_key,
       jitter_s, enabled, last_job_id, next_at, last_run
FROM schedule_legacy;

DROP TABLE schedule_legacy;

CREATE INDEX schedule_enabled ON schedule (enabled);

ALTER TABLE event RENAME TO event_legacy;

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
    trace_json      TEXT    NOT NULL DEFAULT '[]',
    check_observations_json TEXT NOT NULL DEFAULT '[]'
);

INSERT INTO event (id, source, received_at, dedup_key, normalized_json, raw_json,
                   matched_rule, outcome, reason, job_id, trace_json, check_observations_json)
SELECT id, source, received_at, dedup_key, normalized_json, raw_json,
       matched_rule, outcome, reason, job_id, trace_json, check_observations_json
FROM event_legacy;

DROP TABLE event_legacy;

CREATE INDEX event_received_at ON event (received_at, id);
CREATE INDEX event_dedup_key ON event (dedup_key, received_at) WHERE dedup_key <> '';
CREATE INDEX event_job_id ON event (job_id, received_at, id) WHERE job_id IS NOT NULL;

ALTER TABLE rule_fire RENAME TO rule_fire_legacy;

CREATE TABLE rule_fire (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id   INTEGER NOT NULL REFERENCES rule (id) ON DELETE CASCADE,
    ts        INTEGER NOT NULL,
    job_id    INTEGER REFERENCES job (id) ON DELETE SET NULL,
    dedup_key TEXT    NOT NULL DEFAULT ''
);

INSERT INTO rule_fire (id, rule_id, ts, job_id, dedup_key)
SELECT id, rule_id, ts, job_id, dedup_key
FROM rule_fire_legacy;

DROP TABLE rule_fire_legacy;

CREATE INDEX rule_fire_rule_ts ON rule_fire (rule_id, ts);
CREATE INDEX rule_fire_rule_key ON rule_fire (rule_id, dedup_key);

ALTER TABLE workflow_step RENAME TO workflow_step_legacy;

CREATE TABLE workflow_step (
    workflow_id     TEXT    NOT NULL,
    step_no         INTEGER NOT NULL,
    kind            TEXT    NOT NULL,
    status          TEXT    NOT NULL,
    idempotency_key TEXT    NOT NULL,
    job_id          INTEGER REFERENCES job (id) ON DELETE SET NULL,
    result_json     TEXT    NOT NULL DEFAULT '',
    rollback_marker TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    require_approval INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (workflow_id, step_no)
);

INSERT INTO workflow_step (workflow_id, step_no, kind, status, idempotency_key,
                           job_id, result_json, rollback_marker, created_at, updated_at, require_approval)
SELECT workflow_id, step_no, kind, status, idempotency_key,
       job_id, result_json, rollback_marker, created_at, updated_at, require_approval
FROM workflow_step_legacy;

DROP TABLE workflow_step_legacy;

CREATE INDEX workflow_step_incomplete
    ON workflow_step (workflow_id)
    WHERE status IN ('pending', 'running');

ALTER TABLE artifact RENAME TO artifact_legacy;

CREATE TABLE artifact (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id  INTEGER NOT NULL REFERENCES job (id) ON DELETE CASCADE,
    kind    TEXT    NOT NULL,
    path    TEXT    NOT NULL,
    sha256  TEXT    NOT NULL,
    size    INTEGER NOT NULL
);

INSERT INTO artifact (id, job_id, kind, path, sha256, size)
SELECT id, job_id, kind, path, sha256, size
FROM artifact_legacy;

ALTER TABLE approval RENAME TO approval_legacy;

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

INSERT INTO approval (id, job_id, plan_artifact, workspace, plan_serial,
                      requested_at, expires_at, token_hash, state, decided_at, decided_via, reason)
SELECT id, job_id, plan_artifact, workspace, plan_serial,
       requested_at, expires_at, token_hash, state, decided_at, decided_via, reason
FROM approval_legacy;

DROP TABLE approval_legacy;

CREATE INDEX approval_state ON approval (state, expires_at);

DROP TABLE artifact_legacy;

CREATE INDEX artifact_job_id ON artifact (job_id);

DROP TABLE job_legacy;

CREATE INDEX job_state_queued_at ON job (state, queued_at, id);
CREATE INDEX job_concurrency_key ON job (concurrency_key, state);
CREATE INDEX job_dedup_key ON job (dedup_key) WHERE dedup_key IS NOT NULL;
CREATE INDEX job_runbook ON job (runbook);