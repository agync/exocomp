package wire

import "fmt"

// KindApprovalPlan names the structured, redacted plan-diff payload.
const KindApprovalPlan = "approval.plan"

// ApprovalPlanView is the phone approval surface's plan diff as structured JSON:
// the SAME classified, redacted resources the browser plan view renders, never
// the raw sealed blob (which carries secret-backed attribute values the web
// redacts). Destroys is declared first so it marshals first — a phone must see
// what is torn down before anything else. LineCount is the number of rendered
// resource lines; ArtifactID is the sealed plan they came from.
//
// Every value here is already redacted by the shared web classifier: a resource
// whose sealed plan flags anything secret arrives with Redacted set and each
// attribute value replaced by the redaction marker, so this projection can only
// carry what the web would show.
type ApprovalPlanView struct {
	Destroys   []PlanResourceView `json:"destroys"`
	Changes    []PlanResourceView `json:"changes"`
	Creates    []PlanResourceView `json:"creates"`
	Summary    string             `json:"summary"`
	LineCount  int                `json:"line_count"`
	ArtifactID int64              `json:"artifact_id"`
}

// PlanResourceView is one classified resource line: its address, the
// risk-ordered action word, whether it is redacted, and its attribute rows.
type PlanResourceView struct {
	Address  string         `json:"address"`
	Action   string         `json:"action"`
	Redacted bool           `json:"redacted,omitempty"`
	Attrs    []PlanAttrView `json:"attrs"`
}

// PlanAttrView is one attribute row. Value is the marker string, never the
// plaintext, whenever Redacted is set; Before is the pre-apply value the plan
// detail's old→new strike renders (empty when the attribute is new).
type PlanAttrView struct {
	Name     string `json:"name"`
	Before   string `json:"before,omitempty"`
	Value    string `json:"value"`
	Redacted bool   `json:"redacted,omitempty"`
}

// NewApprovalPlanView buckets the already-classified, already-redacted resource
// rows into destroys / changes / creates by action, in the order they were
// classified. It performs no redaction of its own — the rows arrive redacted
// from the web classifier — it only sorts, counts, and summarises. delete and
// replace destroy; create creates; everything else (update) changes.
func NewApprovalPlanView(rows []PlanResourceView, artifactID int64) ApprovalPlanView {
	v := ApprovalPlanView{
		Destroys:   []PlanResourceView{},
		Changes:    []PlanResourceView{},
		Creates:    []PlanResourceView{},
		LineCount:  len(rows),
		ArtifactID: artifactID,
	}
	for _, r := range rows {
		switch r.Action {
		case "delete", "replace":
			v.Destroys = append(v.Destroys, r)
		case "create":
			v.Creates = append(v.Creates, r)
		default:
			v.Changes = append(v.Changes, r)
		}
	}
	v.Summary = fmt.Sprintf("%d to add, %d to change, %d to destroy",
		len(v.Creates), len(v.Changes), len(v.Destroys))
	return v
}
