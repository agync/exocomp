-- ListPlanArtifacts (internal/store/artifact.go) joins job by runbook to find a
-- workspace's saved plan artifacts. Without an index on runbook, that lookup is
-- a full scan of the job table, which only grows — the run ledger is append-only
-- (job_event's no-update/no-delete triggers, migrations 0001/0002) and is never
-- pruned, and artifact rows are never deleted, so it degrades monotonically.

-- runbook is NOT NULL (every job carries one), so a plain index covers every
-- row: an equality lookup is a seek, not a scan.
CREATE INDEX job_runbook ON job (runbook);
