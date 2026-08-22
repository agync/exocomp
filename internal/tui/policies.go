package tui

// policyRow is one CEL plan policy on the policies surface: the seal-time gate
// that decides what an apply may do. A match either denies the plan or routes
// the apply through the approval gate; the expression is shown verbatim so an
// operator sees exactly what gates their applies. Read-only — policies are
// authored via the CLI, which compiles and rejects a bad expression at write.
type policyRow struct {
	name      string
	workspace string
	action    string // deny|require_approval
	enabled   string // "yes"/"no"
	expr      string
}

// PoliciesModel is the plan-policies screen at 120 columns.
type PoliciesModel struct {
	summary string // "plan policies · 2"
	clock   string
	rows    []policyRow
}

const policiesKeys = "read-only · authored with exocomp policy add"

func renderPoliciesScreen(m PoliciesModel) string {
	const w = 120
	lines := []string{
		lr(m.summary, m.clock, w),
		hrule(w),
		row(cell{1, "POLICY"}, cell{19, "WORKSPACE"}, cell{35, "ON MATCH"},
			cell{54, "ENABLED"}, cell{63, "EXPRESSION"}),
	}
	if len(m.rows) == 0 {
		lines = append(lines, "", " no plan policies — applies are gated only by their workspace's approval settings")
	}
	for _, r := range m.rows {
		lines = append(lines, row(
			cell{1, r.name}, cell{19, r.workspace}, cell{35, r.action},
			cell{54, r.enabled}, cell{63, r.expr},
		))
	}
	lines = append(lines, "", lr(policiesKeys, "[0-9] screens", w))
	return join(lines)
}
