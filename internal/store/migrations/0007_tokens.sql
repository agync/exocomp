-- LAN access tokens (Decision 0009, amendment 11 token lifecycle). Tokens are the
-- only credential the network surface accepts; they are minted from local auth
-- (the data dir) and hashed here with argon2id, never stored in the clear.
--
-- The row carries its own lifecycle: an expiry, a last-used stamp, and a
-- revocation time. Revocation is a stamp, not a delete, so a revoked token's
-- name stays reserved and audit rows that reference it still resolve. A browser
-- session is bound to its token row, so revoking the token kills its sessions
-- without a session table of its own.

CREATE TABLE api_token (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    scope        TEXT    NOT NULL CHECK (scope IN ('operator', 'readonly', 'approve')),
    prefix       TEXT    NOT NULL,              -- non-secret display prefix
    hash         TEXT    NOT NULL,              -- argon2id PHC string of the secret
    created_at   INTEGER NOT NULL,             -- unix milliseconds, UTC
    expires_at   INTEGER,                       -- NULL: never expires
    last_used_at INTEGER,
    revoked_at   INTEGER
);

CREATE INDEX api_token_active ON api_token (revoked_at) WHERE revoked_at IS NULL;
