-- Who created a schedule or rule (Decision 0027). A shell schedule or rule
-- runs its argv as the daemon's user with no kernel sandbox, so the row keeps
-- the creating actor, in the words the job ledger uses ("token ci via api",
-- "local CLI operator"). Rows from before this migration read empty: unknown.
ALTER TABLE schedule ADD COLUMN created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE rule ADD COLUMN created_by TEXT NOT NULL DEFAULT '';
