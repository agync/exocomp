package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// enqueueOnly is the driver of the queue an operator write releases work into
// but never drains (AGENTS.md invariant 5). A TUI that approves an approval
// moves its job into queued; the running `exocomp serve` claims that row on
// its own poll, exactly like a CLI-side enqueue. The driver refuses loudly so
// a future caller cannot drain the queue by accident and race the daemon for
// the row — the same invariant cmd/exocomp's enqueueOnly holds.
type enqueueOnly struct{}

// Run implements executor.Driver.
func (enqueueOnly) Run(context.Context, executor.Spec, executor.EventSink) (executor.Result, error) {
	return executor.Result{}, errors.New("queue: the TUI only enqueues; the daemon runs the job")
}

// writeDoneMsg is the outcome of one operator write: a text line to flash and
// the screen whose data must reload. A write that changed the reactive streams
// reloads the glance so the banner and the approval list track it.
type writeDoneMsg struct {
	screen screen
	text   string
}

// decideCmd resolves one approval through the same seam the CLI uses
// (cmd/exocomp/approval.go): the envelope's capability token, an enqueue-only
// queue, a reactive engine over it, and the service's single decision verb. It
// is the one place a TUI answer enters the system, and it never drains.
func decideCmd(st *store.Store, w *Write, approvalID int64, decision string) tea.Cmd {
	return func() tea.Msg {
		env, err := w.OpenEnvelope()
		if err != nil {
			return writeDoneMsg{screen: screenApprovals, text: "decide: " + err.Error()}
		}
		defer func() { _ = env.Close() }()

		ctx := context.Background()
		approval, err := st.GetApproval(ctx, approvalID)
		if err != nil {
			return writeDoneMsg{screen: screenApprovals, text: "decide: " + err.Error()}
		}
		token, _, err := reactive.ApprovalToken(env, approval.JobID)
		if err != nil {
			return writeDoneMsg{screen: screenApprovals, text: "decide: " + err.Error()}
		}
		gate := queue.New(st, enqueueOnly{}, queue.Options{Workers: 1})
		eng, err := reactive.New(reactive.Options{Store: st, Queue: gate, Deriver: env})
		if err != nil {
			return writeDoneMsg{screen: screenApprovals, text: "decide: " + err.Error()}
		}
		svc := service.New(st, w.DataDir, nil)
		res, err := svc.DecideApproval(ctx, eng, service.DecideInput{
			ApprovalID: approvalID, Decision: decision, Token: token, Via: "tui",
		})
		if err != nil {
			// A stale plan is a refusal the screen already renders (the
			// approvals surface computes staleness on every render); reaching
			// the decide with it means the workspace moved in the same moment.
			if errors.Is(err, reactive.ErrStalePlan) {
				return writeDoneMsg{screen: screenApprovals, text: res.Decision.Message}
			}
			return writeDoneMsg{screen: screenApprovals, text: "decide: " + err.Error()}
		}
		return writeDoneMsg{screen: screenApprovals, text: res.Decision.Message}
	}
}

// destroyCheckMsg carries what a plan would destroy — or zero — so the approve
// key knows whether to open the typed-confirmation modal.
type destroyCheckMsg struct {
	approvalID int64
	destroys   int
	resource   string
	err        error
}

// checkDestroyCmd reads a sealed plan artifact through the envelope and counts
// the resources it would destroy. An approval with no plan artifact, or a plan
// that will not parse, reports zero destroys — fail-open is safe here because
// the reactive engine still enforces the token, TTL and stale-plan checks on
// the actual decide; this is only the modal that gates the approve keystroke.
func checkDestroyCmd(st *store.Store, w *Write, approvalID int64) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		a, err := st.GetApproval(ctx, approvalID)
		if err != nil {
			return destroyCheckMsg{approvalID: approvalID, err: err}
		}
		if a.PlanArtifact == nil {
			return destroyCheckMsg{approvalID: approvalID}
		}
		art, err := st.GetArtifact(ctx, *a.PlanArtifact)
		if err != nil {
			return destroyCheckMsg{approvalID: approvalID, err: err}
		}
		env, err := w.OpenEnvelope()
		if err != nil {
			return destroyCheckMsg{approvalID: approvalID, err: err}
		}
		defer func() { _ = env.Close() }()
		blob, err := env.OpenSealedFile(art.Path)
		if err != nil {
			return destroyCheckMsg{approvalID: approvalID, err: err}
		}
		n, first := planDestroySummary(blob)
		return destroyCheckMsg{approvalID: approvalID, destroys: n, resource: first}
	}
}

// planDestroySummary counts the resources a plan would destroy and names the
// first, the facts the typed-confirmation modal renders.
func planDestroySummary(blob []byte) (int, string) {
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(blob, &plan); err != nil {
		return 0, ""
	}
	var n int
	var first string
	for _, rc := range plan.ResourceChanges {
		// A replace (delete+create) tears the resource down too, so it counts
		// as a destroy just like a plain delete.
		if slices.Contains(rc.Change.Actions, "delete") {
			n++
			if first == "" {
				first = rc.Address
			}
		}
	}
	return n, first
}

// diffLoadedMsg carries a decryptable plan diff for the [d] pager.
type diffLoadedMsg struct {
	model ApprovalModel
	err   error
}

// loadApprovalDiffCmd decrypts the approval's plan and renders the
// destroys/changes/creates the pager scrolls. Secret-backed resources are
// marked redacted; their values never reach the model.
func loadApprovalDiffCmd(st *store.Store, w *Write, a store.Approval) tea.Cmd {
	return func() tea.Msg {
		env, err := w.OpenEnvelope()
		if err != nil {
			return diffLoadedMsg{err: err}
		}
		defer func() { _ = env.Close() }()
		if a.PlanArtifact == nil {
			return diffLoadedMsg{err: errors.New("no plan artifact")}
		}
		art, err := st.GetArtifact(context.Background(), *a.PlanArtifact)
		if err != nil {
			return diffLoadedMsg{err: err}
		}
		blob, err := env.OpenSealedFile(art.Path)
		if err != nil {
			return diffLoadedMsg{err: err}
		}
		destroys, changes, creates, secrets := planDiffItems(blob)
		am := ApprovalModel{
			title:     "approvals · plan diff",
			plan:      fmt.Sprintf("PLAN · %s", a.Workspace),
			right:     "expires " + a.ExpiresAt.Local().Format("15:04") + " → auto-REJECT",
			destroys:  destroys,
			changes:   changes,
			creates:   creates,
			freshness: fmt.Sprintf("requested %s ago · plan serial %d", shortDur(time.Since(a.RequestedAt)), a.PlanSerial),
		}
		for i := 0; i < secrets; i++ {
			am.secrets = append(am.secrets, "[redacted · secret]")
		}
		return diffLoadedMsg{model: am}
	}
}

// planDiffItems splits a decrypted plan blob into the ordered, risk-first
// resource lines the diff pager renders, marking secret-backed ones redacted.
func planDiffItems(blob []byte) (destroys, changes, creates []approvalItem, secrets int) {
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions         []string        `json:"actions"`
				AfterSensitive  json.RawMessage `json:"after_sensitive"`
				BeforeSensitive json.RawMessage `json:"before_sensitive"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(blob, &plan); err != nil {
		return nil, nil, nil, 0
	}
	for _, rc := range plan.ResourceChanges {
		item := approvalItem{addr: rc.Address}
		if wire.HasSensitivePlan(rc.Change.AfterSensitive) || wire.HasSensitivePlan(rc.Change.BeforeSensitive) {
			item.detail = "[redacted · secret]"
			secrets++
		}
		switch {
		case slices.Contains(rc.Change.Actions, "delete"):
			destroys = append(destroys, item)
		case slices.Contains(rc.Change.Actions, "create"):
			creates = append(creates, item)
		case slices.Contains(rc.Change.Actions, "update"):
			changes = append(changes, item)
		}
	}
	return destroys, changes, creates, secrets
}

// renderApprovalDiff renders the plan body the [d] pager scrolls: the same
// destroys/changes/creates sections the approval screen shows, in a full-width
// viewport with secret-backed values marked redacted.
func renderApprovalDiff(m ApprovalModel) string {
	const w = 120
	lines := []string{lr(m.title, m.clock, w), hrule(w)}
	if m.plan == "" {
		return join(lines)
	}
	lines = append(lines, lr(m.plan, m.right, w), "")
	for _, sec := range []struct {
		name  string
		items []approvalItem
	}{{"DESTROYS", m.destroys}, {"CHANGES", m.changes}, {"CREATES", m.creates}} {
		if len(sec.items) == 0 {
			continue
		}
		lines = append(lines, sectionHeader(sec.name, len(sec.items)))
		for _, it := range sec.items {
			if it.detail != "" {
				lines = append(lines, row(cell{2, it.addr}, cell{40, it.detail}))
			} else {
				lines = append(lines, "  "+it.addr)
			}
		}
		lines = append(lines, "")
	}
	if len(m.secrets) > 0 {
		lines = append(lines, sectionHeader("SECRETS", len(m.secrets)))
		for _, s := range m.secrets {
			lines = append(lines, "  "+s)
		}
		lines = append(lines, "")
	}
	lines = append(lines, m.freshness)
	return join(lines)
}

// runHistoryLoadedMsg carries a backup target's recent runs for the backups
// screen's enter view.
type runHistoryLoadedMsg struct {
	targetName string
	runs       []store.BackupRun
	err        error
}

func loadRunHistoryCmd(st *store.Store, targetID int64, name string) tea.Cmd {
	return func() tea.Msg {
		runs, err := st.ListBackupRuns(context.Background(), targetID, 20)
		if err != nil {
			return runHistoryLoadedMsg{targetName: name, err: err}
		}
		return runHistoryLoadedMsg{targetName: name, runs: runs}
	}
}

// renderRunHistory draws a target's recent runs, newest first, as the backups
// enter view. State carries the same words the table uses; a still-running leg
// keeps its running state even though it has no finished stamp yet.
func renderRunHistory(target string, runs []store.BackupRun) string {
	const w = 120
	lines := []string{lr("runs · "+target, "[esc] closes", w), hrule(w)}
	if len(runs) == 0 {
		lines = append(lines, "", "  no runs yet for this target")
		return join(lines)
	}
	lines = append(lines, row(
		cell{1, "START"}, cell{21, "STATE"}, cell{33, "SNAPSHOT"},
		cell{49, "BYTES"}, cell{57, "ERROR"}))
	for _, r := range runs {
		lines = append(lines, row(
			cell{1, r.StartedAt.Local().Format("2006-01-02 15:04")},
			cell{21, r.State},
			cell{33, truncate(orDash(r.SnapshotID), 14)},
			cell{49, formatBytes(r.BytesAdded)},
			cell{57, truncate(r.Error, 50)},
		))
	}
	return join(lines)
}

// pauseCmd pauses reactive jobs for the given minutes — a global silence, the
// same kill switch the CLI's silence add --global --for writes.
func pauseCmd(st *store.Store, minutes int) tea.Cmd {
	return func() tea.Msg {
		now := time.Now().UTC()
		_, err := st.CreateSilence(context.Background(), store.Silence{
			ScopeKind: store.ScopeGlobal, Reason: "tui pause", CreatedAt: now,
			ExpiresAt: now.Add(time.Duration(minutes) * time.Minute), CreatedVia: "tui",
		})
		if err != nil {
			return writeDoneMsg{screen: screenHome, text: "pause: " + err.Error()}
		}
		return writeDoneMsg{screen: screenHome, text: fmt.Sprintf("paused for %dm", minutes)}
	}
}

// resumeCmd lifts every global silence the banner is showing.
func resumeCmd(st *store.Store, silences []store.Silence) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		for _, sil := range silences {
			if sil.ScopeKind != store.ScopeGlobal {
				continue
			}
			if err := st.DeleteSilence(ctx, sil.ID); err != nil {
				return writeDoneMsg{screen: screenHome, text: "resume: " + err.Error()}
			}
		}
		return writeDoneMsg{screen: screenHome, text: "resumed"}
	}
}

// muteCmd mutes notifications for one of the modal's expiry presets, logging
// the reason the operator typed. It mirrors the CLI's mute add path.
func muteCmd(st *store.Store, d time.Duration, reason string) tea.Cmd {
	return func() tea.Msg {
		now := time.Now().UTC()
		_, err := st.CreateNotifyMute(context.Background(), store.NotifyMute{
			ScopeKind: store.ScopeGlobal, Reason: reason, CreatedAt: now,
			ExpiresAt: now.Add(d), CreatedVia: "tui",
		})
		if err != nil {
			return writeDoneMsg{screen: screenHome, text: "mute: " + err.Error()}
		}
		return writeDoneMsg{screen: screenHome, text: "muted"}
	}
}

// unmuteCmd lifts every active global notify mute the banner is showing. A
// scoped mute (the web's alert or host silences) is left alone, mirroring
// resumeCmd: the TUI's p/m controls the global kill switches only.
func unmuteCmd(st *store.Store, mutes []store.NotifyMute) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		for _, m := range mutes {
			if m.ScopeKind != store.ScopeGlobal {
				continue
			}
			if err := st.DeleteNotifyMute(ctx, m.ID); err != nil {
				return writeDoneMsg{screen: screenHome, text: "unmute: " + err.Error()}
			}
		}
		return writeDoneMsg{screen: screenHome, text: "unmuted"}
	}
}

// cancelJobCmd moves the selected job to cancelled. The `from` state is read at
// press time; a job that moved meanwhile fails the transition and flashes a
// conflict rather than racing the daemon. Terminal states have nothing to
// cancel, which the caller checks before building the command.
func cancelJobCmd(st *store.Store, id int64, from store.JobState) tea.Cmd {
	return func() tea.Msg {
		err := st.TransitionJob(context.Background(), id, from, store.StateCancelled)
		if err != nil {
			return writeDoneMsg{screen: screenJobs, text: "cancel: " + err.Error()}
		}
		return writeDoneMsg{screen: screenJobs, text: fmt.Sprintf("cancelled job %d", id)}
	}
}

// toggleScheduleCmd flips a schedule's enabled flag.
func toggleScheduleCmd(st *store.Store, name string, enabled bool) tea.Cmd {
	return func() tea.Msg {
		err := st.SetScheduleEnabled(context.Background(), name, !enabled)
		if err != nil {
			return writeDoneMsg{screen: screenSchedules, text: "schedule: " + err.Error()}
		}
		return writeDoneMsg{screen: screenSchedules, text: fmt.Sprintf("%s %s", name, onOff(!enabled))}
	}
}

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// filterRows drops table rows that do not contain the filter, case-folded over
// the row's own text. It is the "start simple" filter: a substring match a
// human scans, not a query language. SGR codes are stripped before matching so
// a numeric filter matches row text, not the colour sequences in styled cells.
func filterRows(rows []table.Row, filter string) []table.Row {
	if filter == "" {
		return rows
	}
	f := strings.ToLower(filter)
	out := make([]table.Row, 0, len(rows))
	for _, r := range rows {
		if strings.Contains(strings.ToLower(stripANSI(strings.Join(r, " "))), f) {
			out = append(out, r)
		}
	}
	return out
}

func (m Model) paused() bool {
	for _, s := range m.silences {
		if s.ScopeKind == store.ScopeGlobal {
			return true
		}
	}
	return false
}

func (m Model) muted() bool { return len(m.mutes) > 0 }

func (m Model) filterable() bool {
	switch m.active {
	case screenJobs, screenBackups, screenWorkspaces, screenSchedules, screenEvents:
		return true
	}
	return false
}

// reloadScreen returns the load command that refreshes the screen a write just
// changed, so the operator sees the result without waiting for the tick.
func (m Model) reloadScreen(s screen) tea.Cmd {
	switch s {
	case screenJobs:
		return loadJobsCmd(m.store)
	case screenBackups:
		return loadBackupsCmd(m.store)
	case screenWorkspaces:
		return loadWorkspacesCmd(m.store)
	case screenSchedules:
		return loadSchedulesCmd(m.store)
	default:
		return loadGlanceCmd(m.store)
	}
}

// applyFilterToActive re-derives the active table's rows from the loaded
// slices under the current filter, immediately after the filter commits.
func (m *Model) applyFilterToActive() {
	switch m.active {
	case screenJobs:
		m.jobsTable.SetRows(filterRows(jobRows(m.jobs), m.filter))
	case screenBackups:
		m.backupsTable.SetRows(filterRows(backupRows(m.targets, m.runs), m.filter))
	case screenWorkspaces:
		m.workspacesTable.SetRows(filterRows(workspaceRows(m.workspaces, m.workspaceStatus), m.filter))
	case screenSchedules:
		m.schedulesTable.SetRows(filterRows(scheduleRows(m.schedules, m.scheduleLast), m.filter))
	}
}

func (m *Model) moveCursorToTop() {
	switch m.active {
	case screenJobs:
		m.jobsTable.model.SetCursor(0)
	case screenBackups:
		m.backupsTable.model.SetCursor(0)
	case screenWorkspaces:
		m.workspacesTable.model.SetCursor(0)
	case screenSchedules:
		m.schedulesTable.model.SetCursor(0)
	case screenApprovals:
		m.approvalCursor = 0
	case screenEvents:
		m.eventCursor = 0
	case screenWorkflows:
		m.workflowsCursor = 0
	}
}

func (m *Model) moveCursorToBottom() {
	switch m.active {
	case screenJobs:
		m.jobsTable.model.GotoBottom()
	case screenBackups:
		m.backupsTable.model.GotoBottom()
	case screenWorkspaces:
		m.workspacesTable.model.GotoBottom()
	case screenSchedules:
		m.schedulesTable.model.GotoBottom()
	case screenApprovals:
		m.approvalCursor = max(0, len(m.approvals)-1)
	case screenEvents:
		m.eventCursor = max(0, len(m.reactiveEvents)-1)
	case screenWorkflows:
		m.workflowsCursor = max(0, len(m.workflows)-1)
	}
}

func (m *Model) moveApprovalCursor(key string) {
	switch key {
	case "up", "k":
		if m.approvalCursor > 0 {
			m.approvalCursor--
		}
	case "down", "j":
		if m.approvalCursor < len(m.approvals)-1 {
			m.approvalCursor++
		}
	}
}

func (m *Model) moveEventCursor(key string) {
	switch key {
	case "up", "k":
		if m.eventCursor > 0 {
			m.eventCursor--
		}
	case "down", "j":
		if m.eventCursor < len(m.reactiveEvents)-1 {
			m.eventCursor++
		}
	}
}

var eventOutcomeCycle = []store.EventOutcome{
	store.OutcomeIgnored, store.OutcomeMatched, store.OutcomeSuppressed,
	store.OutcomeGated, store.OutcomeFired,
}

// cycleEventOutcome advances the events outcome filter: none -> each outcome
// in turn -> none, so a full pass returns to showing every event.
func (m *Model) cycleEventOutcome() {
	for i, o := range eventOutcomeCycle {
		if m.eventOutcome == o {
			if i+1 < len(eventOutcomeCycle) {
				m.eventOutcome = eventOutcomeCycle[i+1]
			} else {
				m.eventOutcome = ""
			}
			return
		}
	}
	m.eventOutcome = eventOutcomeCycle[0]
}

func (m Model) startApprove() (tea.Model, tea.Cmd) {
	if len(m.approvals) == 0 {
		return m, nil
	}
	a := m.approvals[m.approvalCursor]
	// A stale plan never offers approve: the refusal screen is the only way
	// forward, and it leads to a re-plan or a reject.
	if move, stale, err := m.store.ApprovalStaleness(context.Background(), a); err == nil && stale {
		seen := "serial " + itoa64(move.CurrentSerial)
		if !move.MovedAt.IsZero() {
			seen = move.MovedAt.Local().Format("15:04")
		}
		m.stale = StaleModel{workspace: a.Workspace, driftSeen: seen}
		return m, nil
	}
	m.stale = StaleModel{}
	if a.PlanArtifact == nil {
		return m, m.startApproveDecide(a.ID)
	}
	return m, checkDestroyCmd(m.store, m.write, a.ID)
}

func (m Model) startApproveDecide(id int64) tea.Cmd {
	return decideCmd(m.store, m.write, id, reactive.DecisionApprove)
}

func (m Model) startReject() (tea.Model, tea.Cmd) {
	if len(m.approvals) == 0 {
		return m, nil
	}
	a := m.approvals[m.approvalCursor]
	m.stale = StaleModel{}
	return m, decideCmd(m.store, m.write, a.ID, reactive.DecisionReject)
}

func (m Model) openDiff() (tea.Model, tea.Cmd) {
	if len(m.approvals) == 0 {
		return m, nil
	}
	a := m.approvals[m.approvalCursor]
	if move, stale, err := m.store.ApprovalStaleness(context.Background(), a); err == nil && stale {
		_ = move
		// No diff to review against a plan that is no longer current; the
		// refusal screen stands.
		return m, nil
	}
	m.diffOpen = true
	m.diffView.GotoTop()
	if m.write == nil || a.PlanArtifact == nil {
		m.diffView.SetContent(renderApprovalDiff(m.approvalsModel()))
		return m, nil
	}
	m.diffView.SetContent("loading plan diff…")
	return m, loadApprovalDiffCmd(m.store, m.write, a)
}

func (m Model) cancelSelectedJob() (tea.Model, tea.Cmd) {
	idx := m.jobsTable.model.Cursor()
	if idx < 0 || idx >= len(m.jobs) {
		return m, nil
	}
	job := m.jobs[idx]
	if job.State.Terminal() {
		m.flash = fmt.Sprintf("job %d is %s — nothing to cancel", job.ID, job.State)
		return m, nil
	}
	return m, cancelJobCmd(m.store, job.ID, job.State)
}

func (m Model) toggleSelectedSchedule() (tea.Model, tea.Cmd) {
	idx := m.schedulesTable.model.Cursor()
	if idx < 0 || idx >= len(m.schedules) {
		return m, nil
	}
	sc := m.schedules[idx]
	return m, toggleScheduleCmd(m.store, sc.Name, sc.Enabled)
}

func (m Model) openSelectedBackupRuns() (tea.Model, tea.Cmd) {
	idx := m.backupsTable.model.Cursor()
	if idx < 0 || idx >= len(m.targets) {
		return m, nil
	}
	target := m.targets[idx]
	m.runOpen = true
	m.runTarget = target.ID
	m.runName = target.Name
	m.runView.GotoTop()
	m.runView.SetContent("loading runs…")
	return m, loadRunHistoryCmd(m.store, target.ID, target.Name)
}

func (m Model) openSelectedNeed() (tea.Model, tea.Cmd) {
	needs := m.homeModel().needs
	if len(needs) == 0 {
		return m, nil
	}
	switch needs[0].label {
	case "approval":
		m.active = screenApprovals
	case "backup":
		m.active = screenBackups
	default:
		m.active = screenHome
	}
	return m, nil
}

func (m Model) openTrace() (tea.Model, tea.Cmd) {
	if len(m.reactiveEvents) == 0 {
		return m, nil
	}
	e := m.reactiveEvents[m.eventCursor]
	name := e.MatchedRule
	if name == "" {
		name = e.Source
	}
	tr := TraceModel{title: fmt.Sprintf("trace · %s @ %s", truncate(name, 40), e.ReceivedAt.Local().Format("15:04:05"))}
	tr.steps = append(tr.steps, traceStep{
		n:      1,
		text:   fmt.Sprintf("event %s from %s", truncate(name, 24), truncate(e.Source, 12)),
		result: upperOutcome(e.Outcome),
	})
	if e.Reason != "" {
		tr.steps[0].note = "→ " + e.Reason
	}
	m.traceOpen = true
	m.traceContent = renderTrace(tr)
	return m, nil
}

// handleModalKey routes keys while an input modal owns the buffer.
func (m Model) handleModalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.modal = modalNone
		return m, nil
	case "enter":
		return m.submitModal()
	case "backspace":
		if len(m.typed) > 0 {
			m.typed = m.typed[:len(m.typed)-1]
		}
		return m, nil
	case "space":
		m.typed += " "
		return m, nil
	case "left":
		if m.modal == modalMute {
			m.muteOpt = (m.muteOpt + len(mutePresets) - 1) % len(mutePresets)
		}
		return m, nil
	case "right":
		if m.modal == modalMute {
			m.muteOpt = (m.muteOpt + 1) % len(mutePresets)
		}
		return m, nil
	}
	// Printable single characters enter the buffer; control sequences do not.
	t := msg.String()
	if len(t) == 1 && t[0] >= 0x20 && t[0] != 0x7f {
		m.typed += t
	}
	return m, nil
}

func (m Model) submitModal() (tea.Model, tea.Cmd) {
	switch m.modal {
	case modalPause:
		minutes := 30
		if n, err := strconv.Atoi(strings.TrimSpace(m.typed)); err == nil && n > 0 {
			minutes = n
		}
		m.modal = modalNone
		m.flash = ""
		return m, pauseCmd(m.store, minutes)
	case modalMute:
		m.modal = modalNone
		m.flash = ""
		return m, muteCmd(m.store, mutePresets[m.muteOpt], m.typed)
	case modalFilter:
		m.modal = modalNone
		m.flash = ""
		m.filter = strings.TrimSpace(m.typed)
		m.applyFilterToActive()
		return m, nil
	case modalDestroy:
		if m.typed != m.confirmWorkspace {
			m.flash = "type the workspace name exactly to approve"
			return m, nil
		}
		m.modal = modalNone
		m.flash = ""
		return m, m.startApproveDecide(m.confirmApproval)
	}
	return m, nil
}

func (m Model) muteModalView() muteModal {
	d := mutePresets[m.muteOpt]
	until := time.Now().Add(d)
	return muteModal{
		expiry: fmt.Sprintf("for:  15m   1h   4h  until %s", until.Local().Format("15:04")),
		reason: "reason (logged): " + m.typed + "▍",
		until:  until.Local().Format("15:04"),
	}
}

func (m Model) pauseModalView() pauseModal {
	return pauseModal{minutes: m.typed}
}

func (m Model) filterModalView() string {
	screen := "table"
	switch m.active {
	case screenJobs:
		screen = "jobs"
	case screenBackups:
		screen = "backups"
	case screenWorkspaces:
		screen = "workspaces"
	case screenSchedules:
		screen = "schedules"
	case screenEvents:
		screen = "events"
	}
	return join(box(1, 56, []string{
		"filter " + screen,
		"> " + m.typed + "▍",
		"",
		"[enter] filter · [esc] cancel",
	}))
}

func (m Model) destroyConfirmView() string {
	a := m.approval(m.confirmApproval)
	expires := ""
	if a.ID != 0 {
		expires = a.ExpiresAt.Local().Format("15:04")
	}
	return join(box(8, 64, []string{
		"approve plan #" + itoa64(m.confirmApproval) + " — carries " + itoa(m.confirmDestroys) + " DESTROY",
		"destroys " + m.confirmResource,
		"",
		"type the workspace name to approve:",
		"> " + m.typed + "▍",
		"",
		lr("[enter] approve · [esc] cancel", "expires "+expires, 62),
	}))
}

func (m Model) approval(id int64) store.Approval {
	for _, a := range m.approvals {
		if a.ID == id {
			return a
		}
	}
	return store.Approval{}
}

// renderHelpOverlay is the full keymap, shown by [?] and closed by any key. It
// mirrors docs/design/reference/tui/keymap.txt; the write keys deferred to a
// later round are named but inert (see the lane concerns).
func renderHelpOverlay() string {
	const w = 80
	lines := []string{
		lr("exocomp keys", "[?] any key closes", w),
		hrule(w),
		"global",
		"  0-9 [s] [u]  jump to screen     tab / S-tab  next / previous screen",
		"  j k / up down  move             g / G         top / bottom",
		"  enter          open selected     esc           close / back",
		"  /              filter table      ?             this help",
		"  p              pause / resume    m             mute / unmute",
		"  q              quit (root only)",
		"",
		"per screen",
		"  [1] jobs       enter tail · f follow · c cancel · / filter",
		"  [2] backups    enter last runs",
		"  [3] workspaces enter tail · / filter",
		"  [4] schedules  space on/off · / filter",
		"  [5] approvals  a approve · r reject · d diff",
		"  [6] events     e trace · o outcome · / search",
		"",
		"deferred (keys reserved, not wired)",
		"  b run backup · l plan now · x run schedule",
	}
	return join(lines)
}

// spliceOverlay writes the modal box over the base screen at the given row, so
// an operator keeps the screen behind an input instead of losing context.
func spliceOverlay(base, overlay string, at int) string {
	bl := strings.Split(strings.TrimSuffix(base, "\n"), "\n")
	ol := strings.Split(strings.TrimSuffix(overlay, "\n"), "\n")
	for i, line := range ol {
		r := at + i
		if r < 0 || r >= len(bl) {
			continue
		}
		bl[r] = line
	}
	return strings.Join(bl, "\n")
}
