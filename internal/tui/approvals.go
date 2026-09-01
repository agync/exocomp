package tui

// approvalItem is one resource in a plan: a symbol (-, ~, +), its address, and
// an optional detail. A destroy carries its detail on the following indented
// line; a change carries it inline at column 31.
type approvalItem struct {
	addr   string
	detail string
}

// ApprovalModel is the approval-detail screen: risk-ordered, destroys first,
// secrets redacted, and a freshness line proving the plan still matches the
// workspace (the stale-plan check the whole gate turns on).
type ApprovalModel struct {
	title, clock string // "approvals · 1 waiting", "14:03 UTC+11"
	plan, right  string // "PLAN #482 · core-infra · by schedule nightly-plan", "expires 06:41 → auto-REJECT"
	destroys     []approvalItem
	changes      []approvalItem
	creates      []approvalItem
	secrets      []string // "var.db_root_password = [redacted · secret]"
	freshness    string   // "plan age 2m · workspace unchanged since plan ✓"
	// stale, when set, replaces the detail with the fail-closed refusal
	// (renderStalePlan): no approve key exists for a plan whose workspace moved
	// after it was made.
	stale StaleModel
}

const approvalKeys = "[a] approve  [r] reject  [d] diff  [j/k] move"

// renderApprovalDetail draws the 80×24 approval screen. Destroys lead because
// they are the irreversible risk; changes and creates follow; secret values
// are shown redacted so the diff is reviewable without spilling material.
func renderApprovalDetail(m ApprovalModel) string {
	const w = 80
	head := []string{lr(m.title, m.clock, w), hrule(w)}
	if m.plan == "" {
		// No pending approval: the good state, stated plainly (copy.txt:73).
		body := make([]string, 0, 24)
		for i := 0; i < 10; i++ {
			body = append(body, "")
		}
		body = append(body, centerIn("nothing waiting on you", w))
		footer := []string{hrule(w), lr(approvalKeys, "[esc] back", w)}
		return frame(24, head, body, footer)
	}

	var body []string
	body = append(body, lr(m.plan, m.right, w), "")

	body = append(body, sectionHeader("DESTROYS", len(m.destroys)))
	for _, d := range m.destroys {
		body = append(body, "  - "+d.addr)
		if d.detail != "" {
			body = append(body, "      "+d.detail)
		}
	}
	body = append(body, sectionHeader("CHANGES", len(m.changes)))
	for _, c := range m.changes {
		body = append(body, changeRow("~", c))
	}
	body = append(body, sectionHeader("CREATES", len(m.creates)))
	for _, c := range m.creates {
		body = append(body, changeRow("+", c))
	}

	body = append(body, "")
	for i, s := range m.secrets {
		if i == 0 {
			body = append(body, row(cell{0, "SECRETS"}, cell{10, s}))
			continue
		}
		body = append(body, row(cell{10, s}))
	}
	body = append(body, "", m.freshness)

	footer := []string{hrule(w), lr(approvalKeys, "[esc] back", w)}
	return frame(24, head, body, footer)
}

func sectionHeader(name string, n int) string {
	return name + " (" + itoa(n) + ")"
}

// changeRow places the change/create detail at column 31, where the ~ and +
// rows align their right halves.
func changeRow(sym string, c approvalItem) string {
	left := "  " + sym + " " + c.addr
	if c.detail == "" {
		return left
	}
	return row(cell{2, sym + " " + c.addr}, cell{31, c.detail})
}

// StaleModel is the fail-closed refusal: a plan whose workspace moved after it
// was made. There is no approve key — the only way forward is to re-plan.
type StaleModel struct {
	workspace string
	driftSeen string // "14:12"
}

func renderStalePlan(m StaleModel) string {
	lines := []string{
		"PLAN STALE — " + m.workspace + " changed after this plan (drift seen " + m.driftSeen + ")",
		"approval disabled · fail closed · re-plan: exocomp plan " + m.workspace,
		"",
		"[r] reject stale plan  [esc] back",
	}
	return join(lines)
}

// DestroyConfirm is the typed-confirmation gate on a destroy-carrying approval:
// the terminal's hold-to-confirm, mirroring the phone contract. Approval is
// blocked until the operator types the workspace name.
type DestroyConfirm struct {
	planID    string // "482"
	destroys  int
	resource  string // "aws_db_instance.legacy_reports · 210 GB · no snapshot"
	typed     string // what the operator has typed so far ("core-inf")
	expiresAt string // "06:12"
}

func renderDestroyConfirm(d DestroyConfirm) string {
	lines := []string{
		"approve plan #" + d.planID + " — carries " + itoa(d.destroys) + " DESTROY",
		"destroys " + d.resource,
		"",
		"type the workspace name to approve:",
		"> " + d.typed + "▍",
		"",
		lr("[enter] approve · [esc] cancel", "expires "+d.expiresAt, 62),
	}
	return join(box(7, 64, lines))
}
