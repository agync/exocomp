package tui

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/version"
)

// This file wires the fixture-exact renderers (home.go, approvals.go,
// events.go, banner.go) to live store data. The renderers are the contract the
// golden tests hold to; the assembly here fills their view-models from what the
// daemon actually recorded, so the running TUI shows the same shapes over real
// rows.

// glanceLoadedMsg carries everything Home, Approvals, and the banner read that
// the other screens do not already load: pending approvals, recent events, and
// the active holds.
type glanceLoadedMsg struct {
	approvals []store.Approval
	events    []store.Event
	silences  []store.Silence
	mutes     []store.NotifyMute
	swallowed int
	err       error
}

func loadGlanceCmd(st *store.Store) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		approvals, err := st.ListApprovals(ctx, true)
		if err != nil {
			return glanceLoadedMsg{err: err}
		}
		events, err := st.ListEvents(ctx, store.EventFilter{Limit: 20})
		if err != nil {
			return glanceLoadedMsg{err: err}
		}
		silences, err := st.ListSilences(ctx, true)
		if err != nil {
			return glanceLoadedMsg{err: err}
		}
		mutes, err := st.ListNotifyMutes(ctx, true)
		if err != nil {
			return glanceLoadedMsg{err: err}
		}
		// Swallowed is the alerts actually suppressed since the oldest active
		// silence began, not the silence count (matches the web dashboard).
		swallowed := 0
		if len(silences) > 0 {
			earliest := silences[0].CreatedAt
			for _, sil := range silences[1:] {
				if sil.CreatedAt.Before(earliest) {
					earliest = sil.CreatedAt
				}
			}
			if swallowed, err = st.CountSuppressedSince(ctx, earliest); err != nil {
				return glanceLoadedMsg{err: err}
			}
		}
		return glanceLoadedMsg{approvals: approvals, events: events, silences: silences, mutes: mutes, swallowed: swallowed}
	}
}

// bannerState derives the top-line banner from the active holds. A global
// silence is PAUSED; any active notify mute is MUTED; both can be true at once.
func (m Model) bannerState() BannerState {
	now := time.Now()
	s := BannerState{Swallowed: m.swallowed}
	for _, sil := range m.silences {
		if sil.ScopeKind == store.ScopeGlobal {
			s.Paused = true
		} else {
			s.Silenced = true
		}
	}
	if len(m.mutes) > 0 {
		s.Muted = true
		soonest := m.mutes[0]
		for _, mt := range m.mutes[1:] {
			if mt.ExpiresAt.Before(soonest.ExpiresAt) {
				soonest = mt
			}
		}
		s.MutedUntil = soonest.ExpiresAt.Local().Format("15:04")
		s.MutedRemaining = shortDur(soonest.ExpiresAt.Sub(now))
	}
	return s
}

// homeModel assembles the glance screen from every stream the Model holds.
func (m Model) homeModel() HomeModel {
	now := time.Now()
	h := HomeModel{
		title:      "exocomp · " + hostname(),
		clock:      now.Local().Format("15:04 MST"),
		tick:       "tick 2s",
		needsCount: len(m.approvals),
		signal:     m.signalLine(),
		nextLine:   m.nextFireLine(now),
	}
	for _, a := range m.approvals {
		h.needs = append(h.needs, homeNeed{
			label: "approval",
			body:  a.Workspace + " plan",
			right: "expires " + a.ExpiresAt.Local().Format("15:04"),
			tag:   "[5]",
		})
	}
	h.queue = m.queueLine(now)
	for _, t := range m.targets {
		age, status := backupFreshness(m.runs[t.ID], now)
		h.backups = append(h.backups, homeBackup{name: t.Name, age: age, status: status})
	}
	for _, ws := range m.workspaces {
		h.ws = append(h.ws, homeWs{name: ws.Name, status: workspaceGlanceStatus(m.workspaceStatus[ws.ID])})
	}
	return h
}

// approvalsModel renders the pending approval the cursor rests on. The diff
// body (destroys/changes/creates) comes from the sealed plan artifact, which
// this screen does not decrypt; it shows the risk-bearing header and the
// freshness line, and the full diff opens in the pager (the [d] key).
func (m Model) approvalsModel() ApprovalModel {
	now := time.Now()
	title := fmt.Sprintf("approvals · %d waiting", len(m.approvals))
	if len(m.approvals) == 0 {
		return ApprovalModel{title: title, clock: now.Local().Format("15:04 MST")}
	}
	a := m.approvals[0]
	return ApprovalModel{
		title:     title,
		clock:     now.Local().Format("15:04 MST"),
		plan:      fmt.Sprintf("PLAN · %s", a.Workspace),
		right:     "expires " + a.ExpiresAt.Local().Format("15:04") + " → auto-REJECT",
		freshness: fmt.Sprintf("requested %s ago · plan serial %d", shortDur(now.Sub(a.RequestedAt)), a.PlanSerial),
	}
}

// eventsModel renders the recent events with their outcome and reason.
func (m Model) eventsModel() EventsModel {
	now := time.Now()
	counts := map[store.EventOutcome]int{}
	for _, e := range m.reactiveEvents {
		counts[e.Outcome]++
	}
	summary := fmt.Sprintf("events · %d recent · %d ignored · %d matched · %d suppressed · %d gated · %d fired",
		len(m.reactiveEvents),
		counts[store.OutcomeIgnored], counts[store.OutcomeMatched], counts[store.OutcomeSuppressed],
		counts[store.OutcomeGated], counts[store.OutcomeFired])
	em := EventsModel{summary: summary, clock: now.Local().Format("15:04 MST")}
	for i, e := range m.reactiveEvents {
		name := e.MatchedRule
		if name == "" {
			name = e.Source
		}
		em.rows = append(em.rows, eventRow{
			time:     e.ReceivedAt.Local().Format("15:04"),
			event:    truncate(name, 16),
			source:   truncate(e.Source, 10),
			outcome:  upperOutcome(e.Outcome),
			reason:   e.Reason,
			selected: i == 0,
		})
	}
	return em
}

// --- small live-data formatters ---

func (m Model) queueLine(now time.Time) string {
	var running, queued int
	oldest := time.Duration(0)
	for _, j := range m.jobs {
		switch j.State {
		case store.StateRunning:
			running++
		case store.StateQueued:
			queued++
			if d := now.Sub(j.QueuedAt); d > oldest {
				oldest = d
			}
		}
	}
	if running+queued == 0 {
		return "idle · nothing queued"
	}
	return fmt.Sprintf("● %d running · %d queued · oldest %s", running, queued, shortDur(oldest))
}

func (m Model) nextFireLine(now time.Time) string {
	var next *store.Schedule
	for i := range m.schedules {
		s := m.schedules[i]
		if !s.Enabled || s.NextAt == nil {
			continue
		}
		if next == nil || s.NextAt.Before(*next.NextAt) {
			next = &m.schedules[i]
		}
	}
	if next == nil {
		return "no schedule armed"
	}
	return fmt.Sprintf("%s %s · fires in %s", next.NextAt.Local().Format("15:04"), next.Name, shortDur(next.NextAt.Sub(now)))
}

func (m Model) signalLine() string {
	holds := "loud"
	if len(m.mutes) > 0 {
		holds = "muted"
	}
	return fmt.Sprintf("dead-man not wired · notify %s · %s", holds, version.String())
}

func backupFreshness(run store.BackupRun, now time.Time) (age, status string) {
	if run.FinishedAt == nil || run.State != store.BackupSucceeded {
		return "--", "STALE"
	}
	return shortDur(now.Sub(*run.FinishedAt)), "ok"
}

func workspaceGlanceStatus(s workspaceStatus) string {
	if s.Drift {
		return "DRIFT"
	}
	return "in sync"
}

func upperOutcome(o store.EventOutcome) string {
	switch o {
	case store.OutcomeIgnored:
		return "IGNORED"
	case store.OutcomeMatched:
		return "MATCHED"
	case store.OutcomeSuppressed:
		return "SUPPRESSED"
	case store.OutcomeGated:
		return "GATED"
	case store.OutcomeFired:
		return "FIRED"
	default:
		return "ERROR"
	}
}

// shortDur renders a duration the way the glance does: the largest unit only,
// so "2h", "26h", "12d", "4m".
func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

func hostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "exocomp"
}
