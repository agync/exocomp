package tui

// eventRow is one row of the events table: the enum outcome plus the reason
// that outcome carries, which is the whole point of the screen — not that an
// event arrived, but what the pipeline decided and why.
type eventRow struct {
	time     string // "14:02"
	event    string // "disk-pressure"
	source   string // "aurora"
	outcome  string // "SUPPRESSED"
	reason   string
	selected bool
}

// EventsModel is the events screen at 120 columns.
type EventsModel struct {
	summary string // "events · 17 in last 24h · 11 ignored · 2 matched · 3 suppressed · 1 gated · 0 fired"
	clock   string
	rows    []eventRow
}

const eventsKeys = "[e] trace  [o] filter outcome  [/] search  [j/k] move"

// renderEvents draws the events table. The outcome column is fixed-width so the
// enum reads as a column; source and outcome sit one space apart so a long
// source ("core-infra") still leaves the outcome legible.
func renderEventsScreen(m EventsModel) string {
	const w = 120
	lines := []string{
		lr(m.summary, m.clock, w),
		hrule(w),
		row(cell{1, "TIME"}, cell{8, "EVENT"}, cell{26, "SOURCE"}, cell{37, "OUTCOME"}, cell{49, "REASON"}),
	}
	for _, r := range m.rows {
		marker := " "
		if r.selected {
			marker = "▸"
		}
		lines = append(lines, row(
			cell{0, marker}, cell{1, r.time}, cell{8, r.event},
			cell{26, r.source}, cell{37, r.outcome}, cell{49, r.reason},
		))
	}
	lines = append(lines, "", lr(eventsKeys, "[0-9] screens", w))
	return join(lines)
}

// traceStep is one line of the evaluation trace: the guard checked and how it
// resolved, with an optional consequence line under the step that decided the
// outcome.
type traceStep struct {
	n      int
	text   string
	result string
	note   string // indented consequence, e.g. "→ outcome SUPPRESSED · nothing ran · counted on the banner"
}

// TraceModel is the reflowed evaluation trace opened with [e] on an event.
type TraceModel struct {
	title string // "trace · disk-pressure @ 14:02:11"
	steps []traceStep
}

func renderTrace(m TraceModel) string {
	const w = 80
	lines := []string{lr(m.title, "[esc] closes", w), hrule(w)}
	for _, s := range m.steps {
		lines = append(lines, lr(" "+itoa(s.n)+"  "+s.text, s.result, w))
		if s.note != "" {
			lines = append(lines, "    "+s.note)
		}
	}
	return join(lines)
}
