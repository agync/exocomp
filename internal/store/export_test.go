package store

import "context"

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
