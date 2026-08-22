package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"syscall"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// banner reads the active silences into the pause/silence state that rides
// every screen. A global silence is the kill switch (PAUSED); a scoped one is a
// SILENCED banner; both can hold at once.
func (s *Server) banner(r *http.Request) (bannerVM, error) {
	sils, err := s.store.ListSilences(r.Context(), true)
	if err != nil {
		return bannerVM{}, err
	}
	var b bannerVM
	var earliest time.Time
	for _, sil := range sils {
		if sil.ScopeKind == store.ScopeGlobal {
			b.Paused = true
		} else {
			b.Silenced = true
		}
		if earliest.IsZero() || sil.CreatedAt.Before(earliest) {
			earliest = sil.CreatedAt
		}
	}
	if b.Active() {
		n, err := s.store.CountSuppressedSince(r.Context(), earliest)
		if err != nil {
			return bannerVM{}, err
		}
		b.Swallowed = n
	}
	return b, nil
}

// pendingApprovals is the needs-you list: every decision still open, with its
// runbook and fail-closed deadline. A decision past half its window is marked
// expiring, the same threshold the re-publish push fires on.
func (s *Server) pendingApprovals(r *http.Request) ([]approvalRow, error) {
	approvals, err := s.store.ListApprovals(r.Context(), true)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []approvalRow
	for _, a := range approvals {
		row := approvalRow{
			ID: a.ID, JobID: a.JobID, Workspace: a.Workspace,
			Deadline: reactive.ApprovalDeadline(a, now),
		}
		if job, err := s.store.GetJob(r.Context(), a.JobID); err == nil {
			row.Runbook = job.Runbook
		}
		half := a.RequestedAt.Add(a.ExpiresAt.Sub(a.RequestedAt) / 2)
		row.Expiring = !now.Before(half)
		out = append(out, row)
	}
	return out, nil
}

// backups reads each enabled target's newest run into a freshness row on the
// 24h/48h ladder.
func (s *Server) backups(r *http.Request) ([]backupRow, error) {
	targets, err := s.store.ListBackupTargets(r.Context(), false)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []backupRow
	for _, t := range targets {
		row := backupRow{Name: t.Name, Kind: t.Kind, State: "never", Age: "never", Freshness: "fail"}
		runs, err := s.store.ListBackupRuns(r.Context(), t.ID, 1)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			run := runs[0]
			row.State = run.State
			row.Snapshot = run.SnapshotID
			when := run.StartedAt
			if run.FinishedAt != nil {
				when = *run.FinishedAt
			}
			age := now.Sub(when)
			row.Age = humanAge(age)
			row.Freshness = backupFreshness(age)
			if run.State != "succeeded" {
				row.Freshness = "fail"
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// workspaces reports each workspace's serial and whether it has a pending
// drift approval outstanding. A workspace named by an open approval is drifting
// — a plan found changes and is waiting on a human.
func (s *Server) workspaces(r *http.Request, pending []approvalRow) ([]workspaceRow, error) {
	list, err := s.store.ListWorkspaces(r.Context())
	if err != nil {
		return nil, err
	}
	drifting := map[string]bool{}
	for _, a := range pending {
		if a.Workspace != "" {
			drifting[a.Workspace] = true
		}
	}
	var out []workspaceRow
	for _, w := range list {
		out = append(out, workspaceRow{Name: w.Name, Serial: w.Serial, Drift: drifting[w.Name]})
	}
	return out, nil
}

// signal builds the footer's is-the-control-plane-alive strip: newest backup
// age (the dead-man), push transport presence, disk headroom, and version.
func (s *Server) signal(ctx context.Context) SignalLine {
	sig := SignalLine{Version: s.version, DeadMan: "—", Ntfy: "off", Disk: "—"}
	if sig.Version == "" {
		sig.Version = "dev"
	}

	if age, ok := s.newestBackupAge(ctx); ok {
		sig.DeadMan = humanAge(age)
	} else {
		sig.DeadMan = "none"
	}

	if _, err := s.store.GetSecret(ctx, notify.SecretNtfyURL); err == nil {
		sig.Ntfy = "on"
	}

	if s.dataDir != "" {
		if pct, ok := diskFreePercent(s.dataDir); ok {
			sig.Disk = fmt.Sprintf("%d%% free", pct)
			sig.Warn = pct < 10
		}
	}
	return sig
}

func (s *Server) newestBackupAge(ctx context.Context) (time.Duration, bool) {
	targets, err := s.store.ListBackupTargets(ctx, true)
	if err != nil {
		return 0, false
	}
	now := s.now()
	var newest time.Time
	for _, t := range targets {
		runs, err := s.store.ListBackupRuns(ctx, t.ID, 1)
		if err != nil || len(runs) == 0 {
			continue
		}
		when := runs[0].StartedAt
		if runs[0].FinishedAt != nil {
			when = *runs[0].FinishedAt
		}
		if when.After(newest) {
			newest = when
		}
	}
	if newest.IsZero() {
		return 0, false
	}
	return now.Sub(newest), true
}

// diskFreePercent returns the free-space percentage of the filesystem holding
// path.
func diskFreePercent(path string) (int, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 {
		return 0, false
	}
	return int(free * 100 / total), true
}

// eventName pulls a human-facing alert name out of a normalized event body,
// falling back to the source when the body carries none.
func eventName(e store.Event) string {
	var n struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(e.Normalized), &n); err == nil && n.Name != "" {
		return n.Name
	}
	return e.Source
}
