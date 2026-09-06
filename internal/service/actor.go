package service

import (
	"context"
	"encoding/json"
	"fmt"

	"exocomp.dev/exocomp/internal/store"
)

// ActorKindToken names an Actor resolved from an authenticated API/web
// token. ActorKindLocal is available for a face that wants to record the
// local CLI operator explicitly; the zero Actor (Kind == "") also means
// "local CLI operator" and records nothing new, so a caller that never sets
// Actor keeps today's behavior byte-for-byte.
const (
	ActorKindToken = "token"
	ActorKindLocal = "local"
)

// Actor identifies who asked for a write, when the caller is not implicitly
// the local CLI operator. Name is a token's NAME only, never its secret
// (AGENTS.md invariant 4) — api_write.go resolves it from identityOf, the
// same identity its approval-decision path already threads into
// reactive.Decide (apiApprovalDecision).
type Actor struct {
	Kind string
	Name string
	// Face names the surface the write came in on ("api", "mcp"). Empty means
	// the HTTP write API, which is where attribution started.
	Face string
}

// actorEventPayload is the values-free structured payload recorded on the job
// ledger for an attributed write.
type actorEventPayload struct {
	ActorKind string `json:"actor_kind"`
	ActorName string `json:"actor_name"`
	Face      string `json:"face"`
}

// recordActor appends a system job_event naming who asked for a write, the
// same append-only ledger every other job fact already lands on (no schema
// migration, consistent with ADR 0007's audit-surface story). A zero Actor —
// today's CLI default — records nothing and returns nil at once.
//
// When an actor IS present, a failure to append is returned rather than
// logged-and-swallowed the way ordinary run telemetry is (queue/sink.go):
// this record is the only durable evidence of who asked for the write, so a
// write whose attribution silently failed to persist would defeat the point
// of recording it at all.
func (s *Service) recordActor(ctx context.Context, jobID int64, actor Actor) error {
	if actor.Kind == "" {
		return nil
	}
	face := actor.Face
	if face == "" {
		face = "api"
	}
	payload, err := json.Marshal(actorEventPayload{
		ActorKind: actor.Kind,
		ActorName: actor.Name,
		Face:      face,
	})
	if err != nil {
		return fmt.Errorf("service: marshal actor payload: %w", err)
	}
	if _, err := s.store.AppendJobEvent(ctx, store.JobEvent{
		JobID:   jobID,
		Level:   store.LevelSystem,
		Msg:     fmt.Sprintf("enqueued by %s %s via %s", actor.Kind, actor.Name, face),
		RawJSON: string(payload),
	}); err != nil {
		return fmt.Errorf("service: record actor: %w", err)
	}
	return nil
}

// recordPrivileged appends a system job_event naming that a run was requested
// UNSANDBOXED, attributed to whoever asked (the same actor recordActor
// records). It is the attributed, enqueue-time half of the audit; the queue
// emits a second event at execution time on every path. Reusing the ledger
// keeps this a plain job fact with no schema change, and — unlike recordActor —
// it records the local CLI operator too (a zero actor), because a privileged
// run is a deliberate escalation worth naming whoever the face attributes it
// to. A failed append is returned, not swallowed: this is the durable evidence
// the run was authorised to skip the sandbox.
func (s *Service) recordPrivileged(ctx context.Context, jobID int64, actor Actor) error {
	who := "local CLI operator"
	if actor.Kind != "" {
		face := actor.Face
		if face == "" {
			face = "api"
		}
		who = fmt.Sprintf("%s %s via %s", actor.Kind, actor.Name, face)
	}
	if _, err := s.store.AppendJobEvent(ctx, store.JobEvent{
		JobID: jobID,
		Level: store.LevelSystem,
		Msg:   "run requested UNSANDBOXED (privileged) by " + who,
	}); err != nil {
		return fmt.Errorf("service: record privileged: %w", err)
	}
	return nil
}

// JobActor reads back the actor recordActor wrote on a job's ledger, or the
// zero Actor when the write was the local CLI operator's (which records
// nothing). It is what lets a decision path refuse a principal settling the
// approval for a job it asked for itself: the ledger is already
// the durable record of who asked, so no new column is needed to read it back.
func (s *Service) JobActor(ctx context.Context, jobID int64) (Actor, error) {
	events, err := s.store.ListJobEvents(ctx, jobID)
	if err != nil {
		return Actor{}, err
	}
	for _, e := range events {
		if e.RawJSON == "" {
			continue
		}
		var payload actorEventPayload
		if err := json.Unmarshal([]byte(e.RawJSON), &payload); err != nil || payload.ActorKind == "" {
			continue
		}
		return Actor{Kind: payload.ActorKind, Name: payload.ActorName, Face: payload.Face}, nil
	}
	return Actor{}, nil
}
