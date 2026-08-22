-- Recorded SSH host keys for the managed fleet (ADR 0010, credential plane 3).
-- Onboarding records a host's key here so later runs verify it with
-- StrictHostKeyChecking rather than trust-on-first-use into a run. The table
-- lives in the same SQLite store the backup copies wholesale, so known_hosts
-- travels with disaster recovery and is not a loose file to lose.
--
-- One row per (host, key_type): a host may present an ed25519 and an rsa key,
-- and re-recording the same type replaces the value (a legitimate rekey the
-- operator confirms), never silently appends a second.
CREATE TABLE known_host (
    host       TEXT    NOT NULL,
    key_type   TEXT    NOT NULL,
    key        TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (host, key_type)
);
