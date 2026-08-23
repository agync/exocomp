package queue

// WithClaimGate installs a predicate consulted before every claim: while it
// returns false the queue takes no NEW job, so a cordoned daemon stops picking
// up work ahead of a planned reboot. It never touches a job already running —
// draining is letting in-flight work finish, not cancelling it. A nil gate (the
// default) always claims.
func WithClaimGate(gate func() bool) Option {
	return func(q *Queue) { q.claimGate = gate }
}
