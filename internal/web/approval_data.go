package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"exocomp.dev/exocomp/internal/store"
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
		Page:      s.pageFor(r, "Approve", "approvals"),
		ID:        a.ID,
		JobID:     a.JobID,
		Workspace: a.Workspace,
	}
	if job, err := s.store.GetJob(r.Context(), a.JobID); err == nil {
		vm.Runbook = job.Runbook
		vm.Kind = job.Kind
		vm.Provenance = fmt.Sprintf("Job %d (%s) was gated before it can apply; it was triggered %s.",
			job.ID, job.Runbook, job.Trigger)
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
// carried with their value.
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
		return nil, nil, false
	}

	for _, rc := range plan.ResourceChanges {
		action := classifyActions(rc.Change.Actions)
		if action == "no-op" || action == "read" {
			continue
		}
		line := planLine{
			Action:   action,
			Address:  rc.Address,
			Detail:   action,
			Redacted: hasSensitive(rc.Change.AfterSensitive) || hasSensitive(rc.Change.BeforeSensitive),
		}
		if action == "delete" || action == "replace" {
			destroys = append(destroys, line)
		} else {
			changes = append(changes, line)
		}
	}
	return destroys, changes, true
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
// It is a coarse check on purpose: any true anywhere means the phone shows
// [redacted · secret] rather than a value the web UI would not.
func hasSensitive(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	return bytes.Contains(raw, []byte("true"))
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
