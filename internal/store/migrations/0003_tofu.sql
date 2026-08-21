-- OpenTofu state lives here, not in an object store: locking is a row with a
-- UNIQUE primary key rather than a conditional PUT no homelab S3 clone
-- implements correctly (ADR 0004).

CREATE TABLE workspace (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    tofu_dir   TEXT    NOT NULL,
    auto_apply INTEGER NOT NULL DEFAULT 0,
    serial     INTEGER NOT NULL DEFAULT 0
);

-- ciphertext is what tofu POSTed verbatim: an envelope encrypted by tofu's own
-- state encryption. serial and lineage are copied out of its plaintext header
-- so version history is queryable without holding the passphrase.
CREATE TABLE tfstate (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id      INTEGER NOT NULL REFERENCES workspace (id) ON DELETE CASCADE,
    serial            INTEGER NOT NULL,
    lineage           TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    created_by_job_id INTEGER REFERENCES job (id) ON DELETE SET NULL,
    ciphertext        BLOB    NOT NULL
);

CREATE UNIQUE INDEX tfstate_workspace_serial ON tfstate (workspace_id, serial);

-- One row per locked workspace. The primary key is the whole lock: INSERT
-- succeeds for exactly one caller, and SQLite decides which.
CREATE TABLE tflock (
    workspace_id INTEGER PRIMARY KEY REFERENCES workspace (id) ON DELETE CASCADE,
    lock_id      TEXT    NOT NULL,
    who          TEXT    NOT NULL DEFAULT '',
    operation    TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    job_id       INTEGER REFERENCES job (id) ON DELETE SET NULL
);
