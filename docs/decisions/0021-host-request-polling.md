---
type: Decision
title: "Decision 0021 — Keep the host scope write-only; the request response is the polling surface"
description: "The host scope stays write-only: the request response returns terminal state, and a same-key retry polls the original job without enumeration."
status: stable
tags: [decision, api, auth]
---

# Decision 0021 — Keep the host scope write-only; the request response is the polling surface

Status: accepted · 2026-10-14

## Context

A managed host holds a `host`-scoped token and calls `POST
/api/v1/hosts/self/requests` to ask exocomp to run an approved class of job on
itself. The host identity comes from the token, never the body, so a host can
only ever request action on itself. The scope authorizes nothing else: a host
token is refused by every read route, including `GET /jobs` (the write-only
stance `TestAPIHostRequestScopeIsolated` pins).

The question this record settles is whether that stance should stay, or whether
`host` should gain a read so a host can follow up on an asynchronous request.
Two things already existed when the decision was taken:

- The request response is a blocking path. `?wait=true` (or `?wait=1`) makes the
  handler wait until the job reaches a terminal state and return `job_state` and
  `exit_code` in the same response, bounded by `?timeout=` (a Go duration).
- Dedup is keyed on the caller's `Idempotency-Key`. Reissuing the same request
  with the same key is suppressed onto the original event, and the handler looks
  the original decision up so the retry gets the original `job_id` back.

## Decision

**Keep the `host` scope write-only, and make the request response itself the
polling surface.** The blocking path already returns terminal state, so a
synchronous caller needs no read. For an asynchronous follow-up, a host reissues
the same request with the same `Idempotency-Key`: dedup suppression returns the
original `job_id`, and `?wait=true` can be added to that retry to wait on the
original job. That gives poll-without-enumeration — a host follows the outcome of
its own request without ever listing jobs or events.

## Consequences

- A host token gains no read path. Giving `host` a general `canRead` would hand
  every managed host a view of all jobs and events, across every other host and
  every operator action — a fleet-wide disclosure from a credential minted to run
  one class of job on one machine.
- The narrower alternative — a read scoped to "only your own jobs" — is a bigger,
  separate capability: resource-level authorization. There is no per-result
  ownership model to gate it on today (jobs and events are not owned by the host
  that requested them), so it cannot be carved out honestly inside `canRead`.
- The named future step, if a first-class async status path is wanted later, is a
  narrow `GET /api/v1/hosts/self/requests/{id}` (or extending the response above)
  rather than broadening `canRead` for host tokens.
- The response carries two values that are deliberately not `store.JobState`:
  `"pending"` when the deadline passed without a terminal state (an
  approval-gated job lands here, with `approval_id` present), and `"disconnected"`
  when the waiter's connection went away — the job is untouched, so claiming it
  was `cancelled` would be a lie about the job. The wait is bounded by
  `hostRequestMaxWait` (5m); an over-cap `?timeout=` is a `400` naming the
  maximum, never a silent clamp.
