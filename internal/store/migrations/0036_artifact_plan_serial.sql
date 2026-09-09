-- The workspace serial a saved plan was made against. 0 = unknown (pre-migration
-- plans). Lanes C/F read it to render the plan's LIFECYCLE (stale vs current)
-- and the "against serial N" / stale-refusal lines on the plan detail page.
ALTER TABLE artifact ADD COLUMN plan_serial INTEGER NOT NULL DEFAULT 0;