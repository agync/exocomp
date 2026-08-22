-- A notify-layer mute, distinct from a silence. A silence swallows an event
-- before it can become a job (the PAUSED half of the banner). A mute holds the
-- notification while the job it would have announced still runs (the MUTED
-- half). Keeping them in separate tables is what lets the banner show one,
-- the other, or both as genuinely different states.
--
-- expires_at is NOT NULL by the same reasoning as silence: a mute the operator
-- forgets is how a phone goes quiet the week the backups start failing.
CREATE TABLE notify_mute (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scope_kind  TEXT    NOT NULL CHECK (scope_kind IN ('alert', 'host', 'global')),
    scope_value TEXT    NOT NULL DEFAULT '',
    reason      TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    created_via TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX notify_mute_expires_at ON notify_mute (expires_at);
