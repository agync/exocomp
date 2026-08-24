-- EventByJobID (internal/store/reactive.go) is the recovery path that maps a
-- pending approval or a resolve back to the event that raised it (R2). Without
-- an index on job_id, that lookup is a full scan of the event table, which
-- only grows (invariant 4 forbids deleting the ledger). job_id is nullable
-- (most events never name a job), so the index is partial: rows that never
-- fired never dilute it, and the omitted rows can never be the target of this
-- lookup anyway.
CREATE INDEX event_job_id ON event (job_id, received_at, id) WHERE job_id IS NOT NULL;
