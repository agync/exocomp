package store

import (
	"context"
	"time"
)

// BackdateJobForTest moves a job's clock before this process started, so tests
// can produce the row a container restart leaves behind: same pid, older run.
func (s *Store) BackdateJobForTest(id int64, at time.Time) error {
	ms := at.UTC().UnixMilli()
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE job SET queued_at = ?, started_at = CASE WHEN started_at IS NULL
		 THEN NULL ELSE ? END WHERE id = ?`, ms, ms, id)
	return err
}

// SetOwnerPIDForTest rewrites a job's owning pid so tests can produce rows
// that look like they were left behind by a killed process.
func (s *Store) SetOwnerPIDForTest(id int64, pid int) error {
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE job SET owner_pid = ? WHERE id = ?`, pid, id)
	return err
}

// UpdateJobEventForTest attempts an update the API deliberately does not
// offer, so the append-only guard can be asserted from the outside.
func (s *Store) UpdateJobEventForTest(jobID, seq int64, msg string) error {
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE job_event SET msg = ? WHERE job_id = ? AND seq = ?`, msg, jobID, seq)
	return err
}

// PingCapableForTest exposes pingCapable so external test packages can check
// whether the host environment can actually send ICMP.
func PingCapableForTest() error {
	return pingCapable()
}
