-- A plan policy is a CEL rule over a workspace's `tofu show -json` plan output,
-- evaluated when a plan is sealed. `deny` marks the sealed plan unconsumable so
-- a later apply refuses it; `require_approval` routes that apply through the
-- existing approval gate. The expression is validated to compile at `policy
-- add`, so a rule that cannot decide is never stored — it can never silently
-- allow. created_at is INTEGER unix milliseconds (UTC), as elsewhere.

CREATE TABLE plan_policy (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    workspace  TEXT    NOT NULL,
    expr       TEXT    NOT NULL,
    action     TEXT    NOT NULL CHECK (action IN ('deny', 'require_approval')),
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
);

CREATE INDEX plan_policy_workspace ON plan_policy (workspace, enabled);
