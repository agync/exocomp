-- job_event_append_only (0001) blocks UPDATE; this closes the other half of
-- the append-only claim by blocking DELETE too.

CREATE TRIGGER job_event_no_delete
BEFORE DELETE ON job_event
BEGIN
    SELECT RAISE(ABORT, 'job_event is append-only');
END;
