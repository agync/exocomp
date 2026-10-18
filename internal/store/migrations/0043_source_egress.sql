-- Per-source default egress widening for ansible runs (Decision 0023). Empty by
-- default: a source with no declaration keeps the ansible kind's built-in
-- sandbox egress (SSH, 22) exactly as before. The value is a JSON array of TCP
-- ports the operator has deliberately granted, e.g. [443] to let a control-plane
-- play call an API over HTTPS. Mirrors 0021's workspace DDL.
--
-- The migration number is contended: another in-flight lane may take 0043, and
-- it settles at merge.
ALTER TABLE source ADD COLUMN extra_egress_ports TEXT NOT NULL DEFAULT '[]';
