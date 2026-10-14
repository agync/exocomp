package tui

import "exocomp.dev/exocomp/internal/store"

// workflowView is one step-ledger chain (Decision 0008) as the screen holds it: its
// id and the ordered step rows, from which the derived whole-workflow state and
// the done/total tally are read (never stored a second time).
type workflowView struct {
	id    string
	steps []store.WorkflowStep
}

func (w workflowView) state() store.WorkflowState { return store.WorkflowStatus(w.steps) }

func (w workflowView) done() int {
	n := 0
	for _, s := range w.steps {
		if s.Status == store.StepSucceeded {
			n++
		}
	}
	return n
}

// stepGlyph is a step status as a single-width glyph, so a chain's shape reads
// as one strip: the provision->converge and TF->ansible handoffs at a glance.
// Color is a hint layered elsewhere; the glyph carries the state on its own.
func stepGlyph(s store.StepStatus) string {
	switch s {
	case store.StepSucceeded:
		return "✓"
	case store.StepRunning:
		return "●"
	case store.StepFailed:
		return "✗"
	case store.StepBlocked:
		return "▪"
	default: // pending
		return "·"
	}
}

func stepRail(steps []store.WorkflowStep) string {
	r := make([]rune, 0, len(steps))
	for _, s := range steps {
		r = append(r, []rune(stepGlyph(s.Status))...)
	}
	return string(r)
}

// workflowListRow is one chain on the workflows surface.
type workflowListRow struct {
	id       string
	state    string // running|succeeded|failed
	progress string // "3/5"
	rail     string // per-step glyph strip
	selected bool
}

// WorkflowsModel is the workflows screen at 120 columns.
type WorkflowsModel struct {
	summary string // "workflows · 2 recorded"
	clock   string
	rows    []workflowListRow
}

const workflowsKeys = "[enter] open ledger  [j/k] move"

func renderWorkflowsScreen(m WorkflowsModel) string {
	const w = 120
	lines := []string{
		lr(m.summary, m.clock, w),
		hrule(w),
		row(cell{1, "WORKFLOW"}, cell{34, "STATE"}, cell{48, "STEPS"}, cell{56, "LEDGER"}),
	}
	if len(m.rows) == 0 {
		lines = append(lines, "", " no workflows recorded — provision or converge chains appear here")
	}
	for _, r := range m.rows {
		marker := " "
		if r.selected {
			marker = "▸"
		}
		line := row(
			cell{0, marker}, cell{1, r.id}, cell{34, r.state},
			cell{48, r.progress}, cell{56, r.rail},
		)
		if r.selected {
			// The ▸ prefix is the no-color selection contract; the reverse
			// video row is the color hint layered over the same line.
			line = styleSelectedRow.Render(line)
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", lr(workflowsKeys, "[0-9] screens", w))
	return join(lines)
}

// workflowStepLine is one row of a chain's step ledger.
type workflowStepLine struct {
	no     string // "1"
	kind   string // "provision"
	status string // pending|running|succeeded|failed|blocked
	job    string // "#12" or "-"
}

// WorkflowDetailModel is one chain's ordered step ledger, the full detail
// entered with [enter] on the workflows surface.
type WorkflowDetailModel struct {
	id      string
	state   string // derived whole-workflow state
	summary string // "2/4 done"
	steps   []workflowStepLine
}

func renderWorkflowDetail(m WorkflowDetailModel) string {
	const w = 80
	lines := []string{
		lr("workflow · "+m.id, "[esc] back", w),
		hrule(w),
		"state " + m.state + " · " + m.summary,
		"",
		row(cell{2, "STEP"}, cell{8, "KIND"}, cell{28, "STATUS"}, cell{42, "JOB"}),
	}
	for _, s := range m.steps {
		lines = append(lines, row(
			cell{3, s.no}, cell{8, s.kind}, cell{28, s.status}, cell{42, s.job},
		))
	}
	return join(lines)
}
