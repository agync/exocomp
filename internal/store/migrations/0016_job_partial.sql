-- A job that settled as a tolerated degradation rather than a clean success:
-- an unreachable-tolerant ansible run whose only non-success was hosts it could
-- not reach (the roaming laptop asleep), so the reachable fleet converged and the
-- run did NOT fail or page. The row stays in the succeeded terminal state so no
-- state-machine consumer is disturbed; this additive flag distinguishes the
-- degraded success for the CLI, the UI, and a notify treatment, and a job event
-- names which hosts were offline. ADD COLUMN is additive and needs no table
-- rebuild — the default 0 leaves every existing row an ordinary success.
ALTER TABLE job ADD COLUMN partial INTEGER NOT NULL DEFAULT 0;
