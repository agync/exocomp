// Package reactive turns a webhook into a job, or into a documented refusal.
//
// One pipeline: an ingress handler authenticates the sender and normalizes
// whatever it posted into one event shape; enabled rules match it with CEL;
// the guardrails — silence, dedup, cooldown, daily cap, approval — decide in
// that order whether the rule's action reaches the queue, becomes an approval,
// or is swallowed. Every event is persisted with its outcome, its reason, and
// the per-condition trace that produced them, because "why didn't my rule
// fire?" has to be a query.
//
// Nothing here executes anything. The queue is still the only execution
// authority (AGENTS.md invariant 5); this package decides what is allowed to
// reach it.
package reactive
