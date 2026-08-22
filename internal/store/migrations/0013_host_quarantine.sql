-- A managed host whose presented SSH key stopped matching its recorded pin is
-- quarantined (fold 8d): a re-pin at runtime is a security signal, not a
-- transient connection failure. While a host is quarantined the queue blocks
-- its concurrency key, so no run reaches it, until an operator confirms the
-- rekey out of band and re-pins the key through the approval gate
-- (RotateKnownHost), which lifts the quarantine in the same step.
--
-- One row per host. old_fingerprint is the pin that was recorded; new_fingerprint
-- is what the host presented — both travel so the operator sees exactly what
-- changed. Fingerprints, never the raw keys, and no secret: a host key is public.
CREATE TABLE host_quarantine (
    host            TEXT    NOT NULL PRIMARY KEY,
    reason          TEXT    NOT NULL,
    old_fingerprint TEXT    NOT NULL,
    new_fingerprint TEXT    NOT NULL,
    quarantined_at  INTEGER NOT NULL
);
