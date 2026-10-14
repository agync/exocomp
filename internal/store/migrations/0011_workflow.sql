-- The step ledger (Decision 0008): a multi-step choreography is a row per step, its
-- status advanced in the same transaction as the step's own effect. The queue
-- stays the sole execution authority; this table only records where a workflow
-- got to, so a boot resume continues it without re-running a succeeded step.

CREATE TABLE workflow_step (
    workflow_id     TEXT    NOT NULL,
    step_no         INTEGER NOT NULL,
    kind            TEXT    NOT NULL,
    -- pending | running | succeeded | failed | blocked. blocked is a step a
    -- prior failure retired before it ever ran.
    status          TEXT    NOT NULL,
    -- The step's job carries this key, so a resume that re-enqueues resolves to
    -- the same job rather than applying an idempotent effect a second time.
    idempotency_key TEXT    NOT NULL,
    -- The queue job that runs this step. Nullable and SET NULL on delete: the
    -- ledger records the step's own outcome and does not depend on the job row
    -- outliving it.
    job_id          INTEGER REFERENCES job (id) ON DELETE SET NULL,
    result_json     TEXT    NOT NULL DEFAULT '',
    -- Set once a step has an undo point; a rollback choreography reads it. NULL
    -- until the step establishes one.
    rollback_marker TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (workflow_id, step_no)
);

-- The resume sweep asks one question: which workflows have a step still to run?
-- A partial index answers it without scanning the terminal rows, which are the
-- overwhelming majority once workflows finish.
CREATE INDEX workflow_step_incomplete
    ON workflow_step (workflow_id)
    WHERE status IN ('pending', 'running');
