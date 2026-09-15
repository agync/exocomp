-- check_state gains the target's classified netgroup (plan C0/D4): the sweep
-- classifies the probed address once at observation time and stores the
-- result, so the web, the API, and the CLI read one authoritative value
-- instead of each recomputing it against the netgroup table. '' is an http URL
-- target, whose liveness is not a location verdict.
ALTER TABLE check_state ADD COLUMN netgroup TEXT NOT NULL DEFAULT '';
