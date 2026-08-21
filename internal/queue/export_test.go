package queue

// TrackedJobs is how many jobs the queue still holds in the set a Drain claims
// from. A daemon enqueues for as long as it lives, so this has to fall back to
// zero as jobs end rather than only ever growing.
func (q *Queue) TrackedJobs() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.enqueued)
}
