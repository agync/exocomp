-- Per-source default run-secret binding name (Decision 0024). Empty by default:
-- a source with no declaration injects nothing unless the run names a binding,
-- exactly as before. The value is a binding name only; the sealed values it maps
-- stay in run_secret and the envelope and are resolved at execution time.
ALTER TABLE source ADD COLUMN default_run_secrets TEXT NOT NULL DEFAULT '';
