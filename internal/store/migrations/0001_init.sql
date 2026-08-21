-- Timestamps are INTEGER unix milliseconds (UTC); NULL means "not yet".
-- "trigger" is quoted everywhere because it is a SQLite keyword.

CREATE TABLE job (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    runbook         TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    "trigger"       TEXT    NOT NULL,
    state           TEXT    NOT NULL CHECK (state IN (
                        'queued', 'awaiting_approval', 'running', 'succeeded',
                        'failed', 'cancelled', 'timed_out', 'interrupted')),
    args_json       TEXT    NOT NULL DEFAULT '{}',
    queued_at       INTEGER NOT NULL,
    started_at      INTEGER,
    finished_at     INTEGER,
    exit_code       INTEGER,
    concurrency_key TEXT    NOT NULL DEFAULT '',
    dedup_key       TEXT,
    idempotency_key TEXT    UNIQUE,
    owner_pid       INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX job_state_queued_at ON job (state, queued_at, id);
CREATE INDEX job_concurrency_key ON job (concurrency_key, state);
CREATE INDEX job_dedup_key ON job (dedup_key) WHERE dedup_key IS NOT NULL;

CREATE TABLE job_event (
    job_id   INTEGER NOT NULL REFERENCES job (id) ON DELETE CASCADE,
    seq      INTEGER NOT NULL,
    ts       INTEGER NOT NULL,
    level    TEXT    NOT NULL,
    msg      TEXT    NOT NULL,
    raw_json TEXT,
    PRIMARY KEY (job_id, seq)
);

CREATE TRIGGER job_event_append_only
BEFORE UPDATE ON job_event
BEGIN
    SELECT RAISE(ABORT, 'job_event is append-only');
END;

CREATE TABLE artifact (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id  INTEGER NOT NULL REFERENCES job (id) ON DELETE CASCADE,
    kind    TEXT    NOT NULL,
    path    TEXT    NOT NULL,
    sha256  TEXT    NOT NULL,
    size    INTEGER NOT NULL
);

CREATE INDEX artifact_job_id ON artifact (job_id);

CREATE TABLE keyring (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT    NOT NULL,
    recipient   TEXT    NOT NULL,
    wrapped_dek BLOB,
    label       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    revoked_at  INTEGER
);

CREATE UNIQUE INDEX keyring_recipient ON keyring (recipient);

CREATE TABLE secret (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    kind         TEXT    NOT NULL,
    nonce        BLOB    NOT NULL,
    ciphertext   BLOB    NOT NULL,
    aad          TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    rotated_at   INTEGER,
    last_used_at INTEGER
);

CREATE TABLE backup_target (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    name      TEXT    NOT NULL UNIQUE,
    kind      TEXT    NOT NULL,
    uri       TEXT    NOT NULL,
    retention TEXT    NOT NULL DEFAULT '',
    enabled   INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE backup_run (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id   INTEGER NOT NULL REFERENCES backup_target (id) ON DELETE CASCADE,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    state       TEXT    NOT NULL,
    snapshot_id TEXT    NOT NULL DEFAULT '',
    bytes_added INTEGER NOT NULL DEFAULT 0,
    error       TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX backup_run_target_id ON backup_run (target_id, started_at);
