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

// EventsModel is the events screen. width is the terminal width the renderer
// degrades against; zero means the fixed 120-column form the golden pins.
type EventsModel struct {
	summary string // "events · 17 in last 24h · 11 ignored · 2 matched · 3 suppressed · 1 gated · 0 fired"
	clock   string
	rows    []eventRow
	width   int
}

const eventsKeys = "[e] trace  [o] outcome  [/] search  [j/k] move"

// renderEventsScreen draws the events table at the model's width, defaulting to
// the fixed 120-column form the golden pins when the width is unset.
func renderEventsScreen(m EventsModel) string {
	w := m.width
	if w == 0 {
		w = 120
	}
	return renderEventsScreenWidth(m, w)
}

// renderEventsScreenWidth draws the events table at a given width. The outcome
// column is fixed-width so the enum reads as a column; source and outcome sit
// one space apart so a long source ("core-infra") still leaves the outcome
// legible. At width < 120 the REASON cell is clipped with … so the line fits;
// OUTCOME sits before it at a fixed column and is never truncated.
func renderEventsScreenWidth(m EventsModel, w int) string {
	lines := []string{
		lr(m.summary, m.clock, w),
		hrule(w),
		row(cell{1, "TIME"}, cell{8, "EVENT"}, cell{26, "SOURCE"}, cell{37, "OUTCOME"}, cell{49, "REASON"}),
	}
	if len(m.rows) == 0 {
		lines = append(lines, "", "  no events yet — point your alert source at exocomp ingest")
	}
	for _, r := range m.rows {
		marker := " "
		if r.selected {
			marker = "▸"
		}
		text := r.reason
		if w < degradedWidth {
			// The reason starts at column 49; a width too small even for that
			// still must not index the string backwards.
			text = truncate(text, max(0, w-49))
		}
		line := row(
			cell{0, marker}, cell{1, r.time}, cell{8, r.event},
			cell{26, r.source}, cell{37, r.outcome}, cell{49, text},
		)
		if r.selected {
			// The ▸ prefix is the no-color selection contract; the reverse
			// video row is the color hint layered over the same line.
			line = styleSelectedRow.Render(line)
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", lr(eventsKeys, "[?] help", w))
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
