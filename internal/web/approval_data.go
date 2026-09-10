package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// approvalVM builds the phone approval surface for one approval, rendering the
// seven-point contract. A decided or expired approval resolves to a terminal
// notice; a pending one carries its fail-closed countdown, its plan (destroys
// first), its provenance, and — only when the workspace serial moved — the
// stale-plan refusal.
func (s *Server) approvalVM(r *http.Request, id int64, _ string) (approvalVM, error) {
	a, err := s.store.GetApproval(r.Context(), id)
	if err != nil {
		return approvalVM{}, err
	}
	now := s.now()

	vm := approvalVM{
		Page:      s.pageFor(r, "Approvals", "approvals"),
		ID:        a.ID,
		JobID:     a.JobID,
		Workspace: a.Workspace,
	}
	if job, err := s.store.GetJob(r.Context(), a.JobID); err == nil {
		vm.Runbook = job.Runbook
		vm.Kind = job.Kind
		vm.Provenance = fmt.Sprintf("Job %d (%s) was gated before it can apply; it was triggered %s.",
			job.ID, job.Runbook, job.Trigger)
		// The triggering event names the rule that raised the gate: only the
		// normalized name/source/rule, never the raw event body.
		if ev, err := s.store.EventByJobID(r.Context(), job.ID); err == nil && ev.MatchedRule != "" {
			vm.Provenance = fmt.Sprintf("Job %d (%s) was gated after %s event %s matched rule %s.",
				job.ID, job.Runbook, ev.Source, wire.EventName(ev), ev.MatchedRule)
		}
	}

	ident, _ := identityOf(r.Context())

	switch {
	case a.State != store.ApprovalPending:
		vm.Decided = true
		vm.Outcome = string(a.State)
		vm.Notice = decidedNotice(a)
		return vm, nil
	case !now.Before(a.ExpiresAt):
		vm.Expired = true
		return vm, nil
	}

	remaining := a.ExpiresAt.Sub(now)
	vm.Countdown = humanAge(remaining) + " left"
	vm.DeadlineMS = a.ExpiresAt.UnixMilli()
	vm.CanDecide = mayApprove(ident)

	// Stale-plan is a refusal, not a disabled button: if the workspace moved
	// past the plan's serial, the operator re-plans or discards.
	if a.Workspace != "" {
		ws, err := s.store.WorkspaceByName(r.Context(), a.Workspace)
		if err == nil && ws.Serial > a.PlanSerial {
			vm.Stale = true
			vm.StaleNotice = fmt.Sprintf(
				"The plan was made at serial %d and %s is now at serial %d. The plan is kept but cannot be applied — re-plan, or discard it. Nothing was applied.",
				a.PlanSerial, ws.Name, ws.Serial)
		}
	}

	if a.PlanArtifact != nil && s.plans != nil && !vm.Stale {
		destroys, changes, ok := s.renderPlan(r, *a.PlanArtifact)
		if ok {
			vm.HasPlan = true
			vm.Destroys = destroys
			vm.Changes = changes
		}
	}
	return vm, nil
}

// renderPlan reads a sealed plan artifact through the envelope — never the raw
// ciphertext — and splits it into the lines that destroy (rendered first, in
// red) and the rest. Secret-backed attributes are marked redacted, never
// carried with their value. The classification and redaction come from the
// shared classifyPlan core, so the browser plan view and the /api/v1 plan diff
// cannot diverge; renderPlan keeps only the fields the HTML template consumes.
func (s *Server) renderPlan(r *http.Request, artifactID int64) (destroys, changes []planLine, ok bool) {
	art, err := s.store.GetArtifact(r.Context(), artifactID)
	if err != nil {
		return nil, nil, false
	}
	blob, err := s.plans.OpenSealedFile(art.Path)
	if err != nil {
		s.log.Warn("web: open sealed plan", "artifact", artifactID, "err", err)
		return nil, nil, false
	}
	resources, ok := classifyPlan(blob)
	if !ok {
		return nil, nil, false
	}
	for _, rc := range resources {
		line := planLine{Action: rc.Action, Address: rc.Address, Detail: rc.Action, Redacted: rc.Redacted}
		if rc.Action == "delete" || rc.Action == "replace" {
			destroys = append(destroys, line)
		} else {
			changes = append(changes, line)
		}
	}
	return destroys, changes, true
}

// planResource is one classified, redacted resource line of a sealed plan: the
// projection shared by the web approval surface (renderPlan), the /api/v1 plan
// endpoint (apiApprovalPlan), and the plan-detail page (SPEC §4). Redacted
// marks a resource whose sealed plan flags anything secret; its Attrs then
// carry redactedValue in place of every attribute value, so a secret-backed
// value is never carried with its plaintext.
type planResource struct {
	Action   string
	Address  string
	Redacted bool
	Attrs    []planAttr
}

// planAttr is one attribute row with its before and after values. Before is
// empty when the attribute is new, After when it was removed; Same is set when
// the value did not move (the "N attributes unchanged" collapse). Both values
// are redactedValue, never the plaintext, whenever the owning resource is
// redacted.
type planAttr struct {
	Name     string
	Before   string
	After    string
	Redacted bool
	Same     bool
}

// redactedValue is the marker shown in place of a secret-backed value, the same
// text the web plan view renders (view_approval.templ planDetail).
const redactedValue = "[redacted · secret]"

// Changed is the number of attributes whose value moved (Before != After); Same
// the rest. The plan detail's per-resource "N changed · M same" reads them.
func (r planResource) Changed() int {
	n := 0
	for _, at := range r.Attrs {
		if !at.Same {
			n++
		}
	}
	return n
}

func (r planResource) Same() int {
	n := 0
	for _, at := range r.Attrs {
		if at.Same {
			n++
		}
	}
	return n
}

// ActionWord is the keyword the diff table's ACTION cell shows (CREATE · UPDATE
// · REPLACE · DESTROY). no-op/read rows never reach a surface — classifyPlan
// drops them — so the default is unreachable.
func (r planResource) ActionWord() string {
	switch r.Action {
	case "create":
		return "CREATE"
	case "update":
		return "UPDATE"
	case "replace":
		return "REPLACE"
	case "delete":
		return "DESTROY"
	default:
		return "NO-OP"
	}
}

// ActionClass is the keyword's signal: create ok, update/replace wait, delete
// fail. Never colour-only — the keyword word is always shown.
func (r planResource) ActionClass() string {
	switch r.Action {
	case "create":
		return "ok"
	case "update", "replace":
		return "wait"
	case "delete":
		return "fail"
	default:
		return "dim"
	}
}

// classifyPlan parses a decrypted tofu plan blob into the ordered, classified,
// redacted resource list both surfaces build from. It is the one place the
// sensitivity decision (hasSensitive over the *_sensitive trees) and the action
// classification (classifyActions) live; no-op and read resources are dropped
// exactly as renderPlan dropped them. A blob that will not parse yields
// ok=false — the same soft failure renderPlan returned.
func classifyPlan(blob []byte) (resources []planResource, ok bool) {
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions         []string        `json:"actions"`
				Before          map[string]any  `json:"before"`
				After           map[string]any  `json:"after"`
				AfterSensitive  json.RawMessage `json:"after_sensitive"`
				BeforeSensitive json.RawMessage `json:"before_sensitive"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(blob, &plan); err != nil {
		return nil, false
	}
	for _, rc := range plan.ResourceChanges {
		action := classifyActions(rc.Change.Actions)
		if action == "no-op" || action == "read" {
			continue
		}
		redacted := hasSensitive(rc.Change.AfterSensitive) || hasSensitive(rc.Change.BeforeSensitive)
		resources = append(resources, planResource{
			Action:   action,
			Address:  rc.Address,
			Redacted: redacted,
			Attrs:    planAttrs(rc.Change.Before, rc.Change.After, redacted),
		})
	}
	return resources, true
}

// planAttrs projects a resource's before/after attribute maps into
// stable-ordered rows carrying the old→new strike the plan detail renders
// (SPEC §4). The key set is the union of both maps, so a created attribute has
// an empty Before and a removed one an empty After. When the resource is
// redacted every value becomes redactedValue — the coarse, resource-granular
// decision hasSensitive already makes for the whole line — so a secret-backed
// value never appears with its plaintext. Same is decided on the raw values
// before redaction, so a redacted resource still reports its true change count.
func planAttrs(before, after map[string]any, redacted bool) []planAttr {
	keys := map[string]struct{}{}
	for k := range before {
		keys[k] = struct{}{}
	}
	for k := range after {
		keys[k] = struct{}{}
	}
	if len(keys) == 0 {
		return nil
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	attrs := make([]planAttr, 0, len(names))
	for _, name := range names {
		beforeVal, afterVal := attrValue(before[name]), attrValue(after[name])
		a := planAttr{Name: name, Before: beforeVal, After: afterVal, Same: beforeVal == afterVal, Redacted: redacted}
		if redacted {
			a.Before, a.After = redactedValue, redactedValue
		}
		attrs = append(attrs, a)
	}
	return attrs
}

// attrValue renders a non-redacted attribute value as a compact string: a
// string verbatim, a null as empty, anything structured as its JSON encoding.
func attrValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// classifyActions maps a tofu action set to one risk-ordered word.
func classifyActions(actions []string) string {
	has := func(a string) bool {
		for _, x := range actions {
			if x == a {
				return true
			}
		}
		return false
	}
	switch {
	case has("delete") && has("create"):
		return "replace"
	case has("delete"):
		return "delete"
	case has("create"):
		return "create"
	case has("update"):
		return "update"
	case has("read"):
		return "read"
	default:
		return "no-op"
	}
}

// hasSensitive reports whether a plan's *_sensitive tree marks anything secret.
// It delegates to the shared classifier (wire.HasSensitivePlan) so the web,
// TUI, API, and CLI read one decision, never a per-face fork.
func hasSensitive(raw json.RawMessage) bool {
	return wire.HasSensitivePlan(raw)
}

func decidedNotice(a store.Approval) string {
	when := ""
	if a.DecidedAt != nil {
		when = " at " + a.DecidedAt.Format("2006-01-02 15:04:05Z")
	}
	switch a.State {
	case store.ApprovalApproved:
		return fmt.Sprintf("Approved%s. The job is queued and will run. Nothing else to do here.", when)
	case store.ApprovalRejected:
		return fmt.Sprintf("Rejected%s. The job was cancelled; nothing was applied.", when)
	case store.ApprovalExpired:
		return "The decision window closed with no answer. The job was rejected and nothing was applied."
	case store.ApprovalWithdrawn:
		return "The condition cleared before a decision, so this was withdrawn and its job cancelled."
	default:
		return fmt.Sprintf("This decision is settled (%s). Nothing changed.", a.State)
	}
}
