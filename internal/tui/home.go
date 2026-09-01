package tui

import "fmt"

// homeNeed is one NEEDS-YOU row: a label, the body describing what waits, the
// right-justified status, and the screen tag ("[5]", "[2]→b").
type homeNeed struct{ label, body, right, tag string }

// homeRun is the running-job line the wide Home shows under QUEUE.
type homeRun struct{ head, body, right, tag string }

// homeBackup is one BACKUPS row. detail (size · time) shows only on the wide
// Home; the narrow Home drops it.
type homeBackup struct{ name, age, status, detail string }

// homeWs is one WORKSPACES row.
type homeWs struct{ name, status string }

// homeNext is one NEXT-TO-FIRE row, wide Home only.
type homeNext struct{ at, name, when string }

// HomeModel is the glance screen's data. One struct serves both reference
// widths; renderHome80 reads the narrow subset and renderHome120 the rest.
type HomeModel struct {
	title, clock, tick string
	needsCount         int
	needs              []homeNeed
	queue              string
	runs               []homeRun
	backups            []homeBackup
	ws                 []homeWs
	nextLine           string
	next               []homeNext
	// Self-observability: the control plane reporting on itself. version is the
	// running binary (the upgrade target); rpo is the Litestream replication
	// RPO age with the freshness detail beside it.
	selfVersion string
	selfRPO     string
	selfDetail  string
	signal      string
}

// selfLines is the SELF section both Home widths render: the running version
// (what an upgrade replaces) and the Litestream replication RPO age.
func (m HomeModel) selfLines() []string {
	return []string{
		"", "SELF",
		row(cell{2, "version"}, cell{13, m.selfVersion + " · the binary an upgrade replaces"}),
		row(cell{2, "litestream"}, cell{13, "RPO " + m.selfRPO + " · " + m.selfDetail}),
	}
}

const (
	homeKeys80  = "[0-9] [s] [u] screens  [enter] open  [p] pause  [m] mute  [q] quit"
	homeKeys120 = "[0-9] [s] [u] screens  [tab] next  [enter] open  [p] pause  [m] mute  [q] quit"
	helpRight   = "[?] help"
)

func (m HomeModel) needHeader() string { return fmt.Sprintf("NEEDS YOU (%d)", m.needsCount) }

// empty reports the first-day Home: nothing needing attention, no backup
// targets, no workspaces, nothing running, and no schedule armed.
func (m HomeModel) empty() bool {
	return len(m.needs) == 0 && len(m.backups) == 0 && len(m.ws) == 0 &&
		len(m.runs) == 0 && len(m.next) == 0
}

// renderHomeEmpty draws the first-day Home: the empty-state block replaces the
// section skeleton when nothing is configured yet (copy.txt:65-66). The signal
// line still renders in the footer, so the frame keeps observing.
func renderHomeEmpty(m HomeModel, width, height int, keys string) string {
	head := []string{lr(m.title, m.clock+" · "+m.tick, width), hrule(width)}
	body := make([]string, 0, height)
	for i := 0; i < 9; i++ {
		body = append(body, "")
	}
	body = append(body,
		centerIn("nothing configured yet", width),
		"",
		centerIn("the three add commands", width),
		indent(20, "exocomp backup add"),
		indent(20, "exocomp workspace add ./infra"),
		indent(20, "exocomp schedule add"),
	)
	footer := []string{hrule(width), m.signal, lr(keys, helpRight, width)}
	return frame(height, head, body, footer)
}

// centerIn indents s to the horizontal center of a width-wide line.
func centerIn(s string, width int) string { return indent((width-rw(s))/2, s) }

// renderHome80 draws the 80×24 Home.
func renderHome80(m HomeModel) string {
	const w = 80
	if m.empty() {
		return renderHomeEmpty(m, w, 24, homeKeys80)
	}
	head := []string{
		lr(m.title, m.clock+" · "+m.tick, w),
		hrule(w),
	}
	var body []string
	body = append(body, m.needHeader())
	for _, n := range m.needs {
		body = append(body, lr(row(cell{2, n.label}, cell{13, n.body}), n.right+"  "+n.tag, w))
	}
	body = append(body, "", "QUEUE"+"   "+m.queue, "")
	body = append(body, row(cell{0, "BACKUPS"}, cell{44, "WORKSPACES"}))
	rows := len(m.backups)
	if len(m.ws) > rows {
		rows = len(m.ws)
	}
	for i := 0; i < rows; i++ {
		var cs []cell
		if i < len(m.backups) {
			b := m.backups[i]
			cs = append(cs, cell{2, b.name}, cell{20 - rw(b.age), b.age}, cell{23, b.status})
		}
		if i < len(m.ws) {
			ws := m.ws[i]
			cs = append(cs, cell{46, ws.name}, cell{59, ws.status})
		}
		body = append(body, row(cs...))
	}
	body = append(body, "", "NEXT"+"    "+m.nextLine)
	body = append(body, m.selfLines()...)

	footer := []string{hrule(w), m.signal, lr(homeKeys80, helpRight, w)}
	return frame(24, head, body, footer)
}

// renderHome120 draws the 120×36 Home: the same skeleton with a running-job
// line, a size column on the backups, a status detail on the workspaces, and a
// NEXT-TO-FIRE column instead of the single NEXT line.
func renderHome120(m HomeModel) string {
	const w = 120
	if m.empty() {
		return renderHomeEmpty(m, w, 36, homeKeys120)
	}
	head := []string{
		lr(m.title, m.clock+" · "+m.tick, w),
		hrule(w),
	}
	var body []string
	body = append(body, m.needHeader())
	for _, n := range m.needs {
		body = append(body, lr(row(cell{2, n.label}, cell{13, n.body}), n.right+"  "+n.tag, w))
	}
	body = append(body, "", "QUEUE"+"   "+m.queue)
	for _, r := range m.runs {
		body = append(body, lr(row(cell{2, r.head}, cell{27, r.body}), r.right+"  "+r.tag, w))
	}
	body = append(body, "")
	body = append(body, row(cell{0, "BACKUPS"}, cell{44, "WORKSPACES"}, cell{82, "NEXT TO FIRE"}))
	rows := len(m.backups)
	for _, n := range []int{len(m.ws), len(m.next)} {
		if n > rows {
			rows = n
		}
	}
	for i := 0; i < rows; i++ {
		var cs []cell
		if i < len(m.backups) {
			b := m.backups[i]
			cs = append(cs, cell{2, b.name}, cell{21 - rw(b.age), b.age}, cell{24, b.status})
			if b.detail != "" {
				cs = append(cs, cell{30, b.detail})
			}
		}
		if i < len(m.ws) {
			ws := m.ws[i]
			cs = append(cs, cell{46, ws.name}, cell{59, ws.status})
		}
		if i < len(m.next) {
			n := m.next[i]
			cs = append(cs, cell{84, n.at}, cell{95, n.name}, cell{110, n.when})
		}
		body = append(body, row(cs...))
	}
	body = append(body, m.selfLines()...)

	footer := []string{hrule(w), m.signal, lr(homeKeys120, helpRight, w)}
	return frame(36, head, body, footer)
}
