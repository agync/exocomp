-- The check-transition observation ledger (mobile-realign ask 2, checks-round
-- D4/17): the "WHY THIS COUNTED AS A TRANSITION" card renders the N consecutive
-- observations that produced a transition, so the sweep must record them as it
-- goes and the event must carry them permanently (the ledgers it read move on
-- afterwards — the same rule Event.trace_json follows).

-- check_state gains a bounded observation ring per target: the recent
-- {observed_at, status, consecutive} observations the sweep appends each probe
-- and trims. Stored as JSON in a plain TEXT column; it is only ever read whole
-- at emission, never queried by column.
ALTER TABLE check_state ADD COLUMN recent_observations TEXT NOT NULL DEFAULT '[]';

-- event gains the transition's own ledger, embedded at emission so a
-- historical event stays self-sufficient even after the ring moves on.
-- values-free (timestamp + status + consecutive only — no plan or secret).
ALTER TABLE event ADD COLUMN check_observations_json TEXT NOT NULL DEFAULT '[]';