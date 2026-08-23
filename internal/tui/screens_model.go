package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/litestream"
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
	// Self-observability the Home SELF section reads: the running version and
	// the Litestream replication RPO age with its freshness detail.
	selfRPO    string
	selfDetail string
	err        error
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
		rpo, detail := litestreamRPO(ctx, st, time.Now())
		return glanceLoadedMsg{
			approvals: approvals, events: events, silences: silences, mutes: mutes,
			swallowed: swallowed, selfRPO: rpo, selfDetail: detail,
		}
	}
}

// litestreamRPO observes the Litestream replication RPO age the same way the
// metrics registry's staleness gauge does: read the configured replica target,
// then the newest replicated frame's age. An age exocomp cannot observe (no
// replica, a remote replica only litestream can read, or no frames yet) is
// reported honestly as "unknown", never as a fresh-looking zero.
func litestreamRPO(ctx context.Context, st *store.Store, now time.Time) (rpo, detail string) {
	raw, err := st.GetSetting(ctx, litestream.SettingReplicaTarget)
	if err != nil || raw == "" {
		return "unknown", "no replica configured"
	}
	target, perr := litestream.ParseTarget(raw)
	if perr != nil {
		return "unknown", "replica target unparseable"
	}
	age, _, ok, ferr := litestream.ReplicaFreshness(target, now)
	if ferr != nil || !ok {
		if target.Remote() {
			return "unknown", "remote replica — readable only by litestream"
		}
		return "unknown", "no frames replicated yet"
	}
	return shortDur(age), "newest replicated frame"
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
		title:       "exocomp · " + hostname(),
		clock:       now.Local().Format("15:04 MST"),
		tick:        "tick 2s",
		needsCount:  len(m.approvals),
		signal:      m.signalLine(),
		nextLine:    m.nextFireLine(now),
		selfVersion: version.Version,
		selfRPO:     orUnknown(m.selfRPO),
		selfDetail:  orUnknown(m.selfDetail),
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

// sourcesModel assembles the sources screen from the loaded run-tree sources.
func (m Model) sourcesModel() SourcesModel {
	now := time.Now()
	sm := SourcesModel{
		summary: fmt.Sprintf("sources · %d registered", len(m.sources)),
		clock:   now.Local().Format("15:04 MST"),
	}
	for _, s := range m.sources {
		sm.rows = append(sm.rows, sourceRow{
			name:    truncate(s.Name, 18),
			kind:    s.Kind,
			ref:     truncate(orDash(s.Ref), 20),
			sync:    sourceSyncShort(s.LastSyncSHA),
			trust:   sourceTrust(s),
			enabled: yesNo(s.Enabled),
		})
	}
	return sm
}

// sourceTrust names a source's trust posture as a label (never color-only): a
// signed source pinned to its own allowed-signers is the strong form, signed
// alone a step down, unsigned the dim neutral.
func sourceTrust(s store.Source) string {
	switch {
	case s.Signed && s.AllowedSigners != "":
		return "SIGNED · SIGNERS"
	case s.Signed:
		return "SIGNED"
	default:
		return "unsigned"
	}
}

// sourceSyncShort renders a last-sync sha at the length a human scans, never
// the full 40; an unsynced source shows the same "-" every unset cell uses.
func sourceSyncShort(sha string) string {
	if sha == "" {
		return emptyCell
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// policiesModel assembles the plan-policies surface.
func (m Model) policiesModel() PoliciesModel {
	now := time.Now()
	pm := PoliciesModel{
		summary: fmt.Sprintf("plan policies · %d", len(m.policies)),
		clock:   now.Local().Format("15:04 MST"),
	}
	for _, p := range m.policies {
		pm.rows = append(pm.rows, policyRow{
			name:      truncate(p.Name, 16),
			workspace: truncate(orDash(p.Workspace), 14),
			action:    p.Action,
			enabled:   yesNo(p.Enabled),
			expr:      truncate(p.Expr, 56),
		})
	}
	return pm
}

// workflowsModel assembles the workflows surface, marking the cursor row.
func (m Model) workflowsModel() WorkflowsModel {
	now := time.Now()
	wm := WorkflowsModel{
		summary: fmt.Sprintf("workflows · %d recorded", len(m.workflows)),
		clock:   now.Local().Format("15:04 MST"),
	}
	for i, w := range m.workflows {
		wm.rows = append(wm.rows, workflowListRow{
			id:       truncate(w.id, 30),
			state:    string(w.state()),
			progress: fmt.Sprintf("%d/%d", w.done(), len(w.steps)),
			rail:     stepRail(w.steps),
			selected: i == m.workflowsCursor,
		})
	}
	return wm
}

// workflowDetailModel assembles the open chain's step ledger from the
// already-loaded workflow, so the detail refreshes with the list.
func (m Model) workflowDetailModel() WorkflowDetailModel {
	var w workflowView
	for _, cand := range m.workflows {
		if cand.id == m.workflowDetail {
			w = cand
			break
		}
	}
	dm := WorkflowDetailModel{
		id:      m.workflowDetail,
		state:   string(w.state()),
		summary: fmt.Sprintf("%d/%d done", w.done(), len(w.steps)),
	}
	for _, s := range w.steps {
		job := emptyCell
		if s.JobID != nil {
			job = "#" + itoa64(*s.JobID)
		}
		dm.steps = append(dm.steps, workflowStepLine{
			no:     itoa(s.StepNo),
			kind:   truncate(s.Kind, 18),
			status: string(s.Status),
			job:    job,
		})
	}
	return dm
}

// secretsModel assembles the secrets surface from the loaded metadata rows and
// their run-secret bindings. It renders source and the expiry staleness marker
// as labels and the (binding -> env var) breakdown compactly; it never reads the
// nonce, ciphertext or value — those are not even carried into the model.
func (m Model) secretsModel() SecretsModel {
	now := time.Now()
	sm := SecretsModel{
		summary: fmt.Sprintf("secrets · %d stored", len(m.secrets)),
		clock:   now.Local().Format("15:04 MST"),
	}
	for _, s := range m.secrets {
		sm.rows = append(sm.rows, secretRow{
			name:     truncate(s.Name, 22),
			kind:     truncate(orDash(s.Kind), 15),
			source:   secretSource(s.Source),
			created:  secretDate(&s.CreatedAt),
			rotated:  secretDateOrNever(s.RotatedAt),
			lastUsed: secretDateOrNever(s.LastUsedAt),
			expiry:   secretExpiry(s.ExpiresAt, now),
			bindings: secretBindings(m.secretBindings[s.Name]),
		})
	}
	return sm
}

// secretSource labels how a secret was minted. An empty source (older rows) is
// named honestly rather than shown as a blank cell.
func secretSource(src string) string {
	switch src {
	case store.SecretSourceGenerated:
		return "generated"
	case store.SecretSourceUser:
		return "user"
	default:
		return orDash(src)
	}
}

// secretExpiry is the staleness marker: past its expiry reads EXPIRED, a future
// expiry counts down as "expires in Nd", and a NULL expiry reads "no expiry".
// It is a label, never color-only, so an expired secret is legible on its own.
func secretExpiry(exp *time.Time, now time.Time) string {
	if exp == nil {
		return "no expiry"
	}
	if !exp.After(now) {
		return "EXPIRED"
	}
	return "expires in " + shortDur(exp.Sub(now))
}

// secretBindings names the run-secret bindings that inject a secret and the env
// var each maps it to, as "used by: <binding> as $ENV, ...". A secret no binding
// references reads "unbound".
func secretBindings(pairs []store.RunSecret) string {
	if len(pairs) == 0 {
		return "unbound"
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.Binding+" as $"+p.EnvName)
	}
	return "used by: " + strings.Join(parts, ", ")
}

// secretDate renders a lifecycle timestamp as a day, the scale a secret's
// create/rotate/use history is read at.
func secretDate(t *time.Time) string {
	if t == nil || t.IsZero() {
		return emptyCell
	}
	return t.Local().Format("2006-01-02")
}

// secretDateOrNever renders an unset rotate/last-used timestamp as "never"
// rather than the neutral dash, so an un-rotated or never-used secret says so.
func secretDateOrNever(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return secretDate(t)
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
