-- check_state gains the last probe's observed duration in milliseconds, so a
-- target that is up but answering slowly reads as such (scenario S3) without a
-- second probe. 0 is a row the sweep has not observed since this column landed.
ALTER TABLE check_state ADD COLUMN last_duration_ms INTEGER NOT NULL DEFAULT 0;
