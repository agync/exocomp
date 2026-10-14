package service

import "context"

// RecordPrivileged appends a system job_event naming that a run was requested
// UNSANDBOXED, attributed to whoever asked. It exposes recordPrivileged to
// faces that enqueue through queue.Spec directly (the local CLI verbs:
// run-cli, command, ansible). The zero Actor means the local CLI operator.
func (s *Service) RecordPrivileged(ctx context.Context, jobID int64, actor Actor) error {
	return s.recordPrivileged(ctx, jobID, actor)
}
