-- Collapse the three source kinds into two. hostpath and volpath never named a
-- real behavioural difference (outside a container there is no bind mount;
-- inside one a host path IS the data volume), so both become the single kind
-- 'path'. Read/write moves off the kind onto a new writable bit, defaulting to
-- read-only: hostpath was read-only, volpath was read+write, so hostpath maps to
-- writable=0 and volpath to writable=1, preserving prior semantics exactly.
--
-- SQLite cannot narrow a CHECK in place, so the table is rebuilt. Unlike the
-- schedule rebuilds (0015/0020), inventory references source with ON DELETE
-- CASCADE, and a plain DROP of source performs an implicit DELETE that cascades
-- inventory away — foreign_keys cannot be turned off inside the migration
-- transaction to stop it. So source is renamed aside (which repoints inventory's
-- FK to the renamed table), the new source is built and repopulated, inventory
-- is rebuilt to reference the new source, and only then is the old source
-- dropped, by which point nothing references it. defer_foreign_keys holds
-- enforcement to commit, when every source_id again resolves.
PRAGMA defer_foreign_keys = ON;

ALTER TABLE source RENAME TO source_legacy;

CREATE TABLE source (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT    NOT NULL UNIQUE,
    kind              TEXT    NOT NULL CHECK (kind IN ('git', 'path')),
    -- git: the clone URL. path: the tree's path, absolute or relative to the
    -- data volume. Never a secret.
    uri               TEXT    NOT NULL DEFAULT '',
    -- git: the ref/tag the control plane pins to and advances only after a gated
    -- run. Empty for a path source.
    ref               TEXT    NOT NULL DEFAULT '',
    deploy_key_secret TEXT    NOT NULL DEFAULT '',
    last_sync_sha     TEXT    NOT NULL DEFAULT '',
    signed            INTEGER NOT NULL DEFAULT 0,
    allowed_signers   TEXT    NOT NULL DEFAULT '',
    -- 1 grants a path source's tree a read+write working directory; 0 (the
    -- default) grants read only. Ignored for git, whose checkout is managed.
    writable          INTEGER NOT NULL DEFAULT 0,
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        INTEGER NOT NULL
);

INSERT INTO source (id, name, kind, uri, ref, deploy_key_secret, last_sync_sha,
                    signed, allowed_signers, writable, enabled, created_at)
SELECT id, name,
       CASE kind WHEN 'hostpath' THEN 'path' WHEN 'volpath' THEN 'path' ELSE kind END,
       uri, ref, deploy_key_secret, last_sync_sha, signed, allowed_signers,
       CASE kind WHEN 'volpath' THEN 1 ELSE 0 END,
       enabled, created_at
FROM source_legacy;

ALTER TABLE inventory RENAME TO inventory_legacy;

CREATE TABLE inventory (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    name             TEXT    NOT NULL UNIQUE,
    source_id        INTEGER REFERENCES source (id) ON DELETE CASCADE,
    path             TEXT    NOT NULL DEFAULT '',
    kind             TEXT    NOT NULL CHECK (kind IN ('static', 'dynamic')),
    env_binding_json TEXT    NOT NULL DEFAULT '{}',
    created_at       INTEGER NOT NULL
);

INSERT INTO inventory (id, name, source_id, path, kind, env_binding_json, created_at)
SELECT id, name, source_id, path, kind, env_binding_json, created_at
FROM inventory_legacy;

DROP TABLE inventory_legacy;
DROP TABLE source_legacy;
