package queue

// TrackedJobs is how many jobs the queue still holds in the set a Drain claims
// from. A daemon enqueues for as long as it lives, so this has to fall back to
// zero as jobs end rather than only ever growing.
func (q *Queue) TrackedJobs() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.enqueued)
}

// ClaimPageSize is claim's page size, exported so a test can size a window of
// blocked rows against the real value rather than a copy that could drift.
const ClaimPageSize = claimPageSize

// RelativizeArgv exports relativizeArgv for its own unit test.
func RelativizeArgv(argv []string, dir string) []string { return relativizeArgv(argv, dir) }
