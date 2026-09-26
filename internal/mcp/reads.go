package mcp

import (
	"context"
	"errors"
	"strconv"

	"exocomp.dev/exocomp/internal/checkprobe"
	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// These are the one set of read functions both a tool and a resource call, so a
// resource's bytes and its tool's bytes come from the same store read and the
// same wire projection — they cannot diverge. Each returns (kind, data) for the
// wire envelope, or an error the caller maps onto the taxonomy.

// Local kinds for projections the shared wire package does not name. They follow
// the wire naming (entity.verb) and, like the HTTP write API's local view kinds,
// carry identifiers and states only.
const (
	kindWorkspaceGet   = "workspace.get"
	kindWorkspaceDrift = "workspace.drift"
	kindEventExplain   = "event.explain"
)

func (s *Server) readJobs(ctx context.Context, filter store.JobFilter) (string, any, error) {
	rows, err := s.store.ListJobs(ctx, filter)
	if err != nil {
		return "", nil, err
	}
	return wire.KindJobList, wire.NewJobViewsResolved(ctx, s.store, rows), nil
}

func (s *Server) readJob(ctx context.Context, id int64) (string, any, error) {
	job, err := s.store.GetJob(ctx, id)
	if err != nil {
		return "", nil, err
	}
	return wire.KindJob, wire.NewJobViewResolved(ctx, s.store, job), nil
}

// readJobEvents projects a job's append-only, already-redacted event log,
// optionally only events after a sequence cursor so an agent can tail without
// re-reading. It filters store rows before projecting, so the bytes stay from
// the same marshaller (matching internal/web apiJobEvents).
func (s *Server) readJobEvents(ctx context.Context, id, since int64) (string, any, error) {
	rows, err := s.store.ListJobEvents(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if since > 0 {
		kept := rows[:0]
		for _, e := range rows {
			if e.Seq > since {
				kept = append(kept, e)
			}
		}
		rows = kept
	}
	return wire.KindJobEvents, wire.NewJobEventList(rows), nil
}

func (s *Server) readWorkspaces(ctx context.Context) (string, any, error) {
	rows, err := s.store.ListWorkspaces(ctx)
	if err != nil {
		return "", nil, err
	}
	return wire.KindWorkspaceList, wire.NewWorkspaceList(rows), nil
}

// readWorkspace projects one workspace. It reuses NewWorkspaceList so a single
// get and a one-element list project a row identically.
func (s *Server) readWorkspace(ctx context.Context, name string) (string, any, error) {
	ws, err := s.store.WorkspaceByName(ctx, name)
	if err != nil {
		return "", nil, err
	}
	return kindWorkspaceGet, wire.NewWorkspaceList([]store.Workspace{ws})[0], nil
}

func (s *Server) readSources(ctx context.Context) (string, any, error) {
	rows, err := s.store.ListSources(ctx)
	if err != nil {
		return "", nil, err
	}
	return wire.KindSourceList, wire.NewSourceList(rows), nil
}

// readApprovals lists approvals; pendingOnly mirrors `approval list`'s default.
func (s *Server) readApprovals(ctx context.Context, pendingOnly bool) (string, any, error) {
	rows, err := s.store.ListApprovals(ctx, pendingOnly)
	if err != nil {
		return "", nil, err
	}
	return wire.KindApprovalList, wire.NewApprovalList(rows), nil
}

func (s *Server) readSchedules(ctx context.Context) (string, any, error) {
	rows, err := s.store.ListSchedules(ctx, false)
	if err != nil {
		return "", nil, err
	}
	return wire.KindScheduleList, wire.NewScheduleList(rows), nil
}

func (s *Server) readRules(ctx context.Context) (string, any, error) {
	rows, err := s.store.ListRules(ctx, false)
	if err != nil {
		return "", nil, err
	}
	return wire.KindRuleList, wire.NewRuleList(rows), nil
}

func (s *Server) readEvents(ctx context.Context, filter store.EventFilter) (string, any, error) {
	rows, err := s.store.ListEvents(ctx, filter)
	if err != nil {
		return "", nil, err
	}
	return wire.KindEventList, wire.NewEventList(rows), nil
}

// readChecks projects every check with its targets' last observed status,
// stored netgroup, probe duration, and recency — the same wire bytes
// `check list --output json` and GET /api/v1/checks emit.
func (s *Server) readChecks(ctx context.Context) (string, any, error) {
	rows, err := s.store.ListChecks(ctx, false)
	if err != nil {
		return "", nil, err
	}
	states, err := s.checkStates(ctx, rows)
	if err != nil {
		return "", nil, err
	}
	return wire.KindCheckList, wire.NewCheckList(rows, states), nil
}

// readCheck projects one check by name. It reuses NewCheckList so a single get
// and a one-element list project a row identically.
func (s *Server) readCheck(ctx context.Context, name string) (string, any, error) {
	c, err := s.store.GetCheck(ctx, name)
	if err != nil {
		return "", nil, err
	}
	states, err := s.checkStates(ctx, []store.Check{c})
	if err != nil {
		return "", nil, err
	}
	return wire.KindCheck, wire.NewCheckList([]store.Check{c}, states)[0], nil
}

// checkStates loads each check's per-target state rows, the join NewCheckList
// takes.
func (s *Server) checkStates(ctx context.Context, rows []store.Check) (map[int64][]store.CheckState, error) {
	states := make(map[int64][]store.CheckState, len(rows))
	for _, c := range rows {
		st, err := s.store.ListCheckStates(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		states[c.ID] = st
	}
	return states, nil
}

func (s *Server) readNetgroups(ctx context.Context) (string, any, error) {
	rows, err := s.store.ListNetgroups(ctx)
	if err != nil {
		return "", nil, err
	}
	return wire.KindNetgroupList, wire.NewNetgroupList(rows), nil
}

// probeCheck runs a stored check's probes immediately, with the sweep's options,
// and writes nothing: no check_state row moves and no event is emitted (D3). It
// is how a check is debugged without waiting an interval. only narrows to one
// target by name.
func (s *Server) probeCheck(ctx context.Context, name, only string) (string, any, error) {
	c, err := s.store.GetCheck(ctx, name)
	if err != nil {
		return "", nil, err
	}
	results, err := checkprobe.Run(ctx, s.store, c, only)
	if err != nil {
		if errors.Is(err, checkprobe.ErrNoTarget) {
			return "", nil, invalidArg("check " + name + " has no target named " + strconv.Quote(only))
		}
		return "", nil, invalidArg(err.Error())
	}
	views := make([]wire.CheckProbeTargetView, 0, len(results))
	for _, r := range results {
		views = append(views, wire.CheckProbeTargetView{
			Target:     r.Name,
			Up:         r.Up,
			DurationMS: r.DurationMS,
			Netgroup:   r.Netgroup,
			Detail:     r.Detail,
		})
	}
	return wire.KindCheckProbe, wire.NewCheckProbeView(name, views), nil
}

// readSecrets returns secret METADATA only. It reuses wire.SecretView, whose
// type has no nonce/ciphertext/AAD field: a sealed value cannot leak through
// this path even by accident (invariant 4).
func (s *Server) readSecrets(ctx context.Context) (string, any, error) {
	rows, err := s.store.ListSecrets(ctx)
	if err != nil {
		return "", nil, err
	}
	return wire.KindSecretList, wire.NewSecretList(rows), nil
}

func (s *Server) readBackups(ctx context.Context) (string, any, error) {
	rows, err := s.store.ListBackupTargets(ctx, true)
	if err != nil {
		return "", nil, err
	}
	return wire.KindBackupList, wire.NewBackupTargetList(rows), nil
}

// readHealth runs the same subsystem probes `doctor --output json` and
// /api/v1/health run, over the server's already-open store. It never fails: an
// unreachable subsystem is a per-entry status, not an error.
func (s *Server) readHealth(ctx context.Context) (string, any, error) {
	return wire.KindDoctor, health.Probe(ctx, s.dataDir, s.healthOpenStore), nil
}

// healthOpenStore hands internal/health the server's already-open store rather
// than a fresh connection, since the MCP server is long-lived like the daemon.
func (s *Server) healthOpenStore() (health.Store, func() error, error) {
	return s.store, func() error { return nil }, nil
}
