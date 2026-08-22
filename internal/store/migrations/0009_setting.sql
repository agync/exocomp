-- A small key/value store for non-secret operator configuration the setup
-- wizard and the surfaces read back: the operator display name, the generated
-- ntfy topic label, and the like. Secret material never lands here — that stays
-- in the sealed secret table and the envelope. Values are plain text so the row
-- is readable in a bare SQLite session, per the no-bespoke-format invariant.
CREATE TABLE setting (
    key        TEXT PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);
