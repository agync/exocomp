-- last_run records when a schedule last fired (INTEGER unix ms, UTC; NULL means
-- never fired). The scheduler reads it on (re)load to coalesce a single
-- catch-up fire for a schedule whose cron had an occurrence between last_run and
-- now while the daemon was down. NULL suppresses catch-up entirely, so a
-- brand-new schedule only ever fires forward. Adding one nullable column needs
-- no table rebuild — unlike the kind-CHECK widenings, no constraint changes.
ALTER TABLE schedule ADD COLUMN last_run INTEGER;
