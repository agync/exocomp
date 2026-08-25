-- UnifiedPush per-device push endpoints (v1.1 push path). A device registers
-- the URL of its UnifiedPush distributor; the daemon POSTs each notification to
-- it. The URL is CLIENT-SUPPLIED and the daemon connects to it, so both the
-- registration path and the send path refuse any endpoint that resolves to a
-- loopback, private, link-local, ULA, or cloud-metadata address (SSRF).
--
-- token_id/token_name attribute a row to the operator token that registered it,
-- so a device manages only its own endpoints. Revocation is a stamp on the token
-- row, not a delete, so this table carries no foreign key: a revoked token's
-- endpoints are simply orphaned, and stay deletable by id.
CREATE TABLE push_endpoint (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    token_id        INTEGER NOT NULL,
    token_name      TEXT    NOT NULL DEFAULT '',
    url             TEXT    NOT NULL,
    label           TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    last_success_at INTEGER,
    last_error      TEXT
);

CREATE INDEX push_endpoint_token ON push_endpoint (token_id);
