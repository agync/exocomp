-- Opt-in check-level aggregate (design §7.4, D5). When aggregate is set the
-- sweep derives one check-level transition from the present target set in
-- addition to the per-target ones (see reactive.md). min_up is that rollup's
-- threshold: the number of targets that must read up for the aggregate to read
-- up, with 0 meaning every target. It is bounded to 0..len(targets) at write
-- time in Go; default 0 with aggregate off leaves every existing check's
-- behaviour unchanged.
ALTER TABLE check_row ADD COLUMN aggregate INTEGER NOT NULL DEFAULT 0;
ALTER TABLE check_row ADD COLUMN min_up INTEGER NOT NULL DEFAULT 0;
