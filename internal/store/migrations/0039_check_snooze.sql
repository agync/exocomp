-- check_row gains a check-level snooze with a mandatory expiry: while
-- snoozed_until is in the future the sweep still probes targets and advances
-- their state, but emits no transitions. 0 is not snoozed. It lives on the row
-- (a check-level mute), not on check_state (per-target observation).
ALTER TABLE check_row ADD COLUMN snoozed_until INTEGER NOT NULL DEFAULT 0;
