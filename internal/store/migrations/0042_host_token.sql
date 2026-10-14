-- Host-scoped request credentials (host-initiated job requests). A host token
-- authenticates only POST /api/v1/hosts/self/requests and carries the canonical
-- hostname it belongs to, so a request acts on the requesting host and not on
-- an operator-provided target. Scope CHECK widens to admit 'host'; the host
-- column binds the row to one machine. Existing rows keep NULL host.
CREATE TABLE api_token_new (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    scope        TEXT    NOT NULL CHECK (scope IN ('operator', 'readonly', 'approve', 'host')),
    prefix       TEXT    NOT NULL,
    hash         TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    last_used_at INTEGER,
    revoked_at   INTEGER,
    rotated_at   INTEGER,
    host         TEXT
);
INSERT INTO api_token_new (id, name, scope, prefix, hash, created_at,
                           expires_at, last_used_at, revoked_at, rotated_at, host)
SELECT id, name, scope, prefix, hash, created_at,
       expires_at, last_used_at, revoked_at, rotated_at, NULL
FROM api_token;
DROP TABLE api_token;
ALTER TABLE api_token_new RENAME TO api_token;
CREATE INDEX api_token_active ON api_token (revoked_at) WHERE revoked_at IS NULL;
