package web

import (
	"errors"
	"net/http"
	"strings"

	"exocomp.dev/exocomp/internal/policy"
	"exocomp.dev/exocomp/internal/store"
)

// A plan policy carries no secret material — it is a name, a workspace, a CEL
// expression, and an action — so this lane is operator-scope only, like
// schedules: no reauth, no confirm_token. add mirrors `exocomp policy add`
// (cmd/exocomp/policy.go): the expression is compiled here so a policy that
// could never decide is refused now rather than silently failing a later seal.
// Refusals re-render the dialog at 422 with every field's message at once
// (forms/SPEC.md); the raw validator words go to the log, never the operator.

func init() { registerWrite((*Server).registerPolicyWrite) }

func (s *Server) registerPolicyWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /policies/add", s.requireAuth(s.handlePolicyAdd))
	mux.HandleFunc("POST /policies/{name}/rm", s.requireAuth(s.handlePolicyRm))
}

// The add dialog's human refusal copy (forms/copy.txt): one sentence in the
// field's terms, never the validator's words or a status code.
const (
	policyWorkspaceRequired = "Give the workspace a name."
	policyActionRefusal     = "On match is deny or require_approval."
	policyExprRefusal       = "The expression can't compile — check the CEL syntax."
	policySubmitRefusal     = "Couldn't add the policy — nothing was saved."
)

func policyNameRefusal(name string) (msg, fix string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Give the policy a name.", ""
	}
	if strings.Contains(name, " ") {
		msg = "Names can't have spaces."
	} else {
		msg = "Names use letters, digits, dots, hyphens and underscores."
	}
	return msg, netgroupSlug(name)
}

func policyDuplicateRefusal(name string) string {
	return "A policy named " + name + " already exists."
}

func (s *Server) handlePolicyAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	form := policyFormVM{
		Name: r.PostFormValue("name"), Workspace: r.PostFormValue("workspace"),
		Action: r.PostFormValue("action"), Expr: r.PostFormValue("expr"),
	}

	// Validate every field up front so all refusals render at once (forms/
	// SPEC.md); the daemon stays authoritative on uniqueness.
	var causes []string
	if !store.ValidName(form.Name) {
		form.ErrName, form.FixName = policyNameRefusal(form.Name)
		causes = append(causes, "name "+form.Name)
	}
	if form.Workspace == "" {
		form.ErrWorkspace = policyWorkspaceRequired
		causes = append(causes, "empty workspace")
	}
	switch form.Action {
	case store.PolicyDeny, store.PolicyRequireApproval:
	default:
		form.ErrAction = policyActionRefusal
		causes = append(causes, "action "+form.Action)
	}
	if _, err := policy.Compile(form.Expr); err != nil {
		form.ErrExpr = policyExprRefusal
		causes = append(causes, err.Error())
	}

	if form.ErrName != "" || form.ErrWorkspace != "" || form.ErrAction != "" || form.ErrExpr != "" {
		switch {
		case form.ErrName != "":
			form.FirstInvalid = "name"
		case form.ErrWorkspace != "":
			form.FirstInvalid = "workspace"
		case form.ErrAction != "":
			form.FirstInvalid = "action"
		case form.ErrExpr != "":
			form.FirstInvalid = "expr"
		}
		s.log.Info("web: policy add refused", "err", strings.Join(causes, "; "), "by", ident.token.Name)
		s.policyRefusal(w, r, form)
		return
	}

	created, err := s.store.CreatePlanPolicy(r.Context(), store.PlanPolicy{
		Name: form.Name, Workspace: form.Workspace, Expr: form.Expr, Action: form.Action,
	})
	if errors.Is(err, store.ErrDuplicate) {
		s.log.Info("web: policy add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrName = policyDuplicateRefusal(form.Name)
		form.FirstInvalid = "name"
		s.policyRefusal(w, r, form)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: policy add", "policy", created.Name, "workspace", created.Workspace,
		"action", created.Action, "by", ident.token.Name)
	s.redirectFlash(w, r, "/policies", "added "+created.Name)
}

// policyRefusal re-renders the policies surface at 422 with the add dialog
// carrying its refusals — the JS-off path, and the fragment body a JS submit
// swaps in place. Refusals never flash: flash is success-only (write.go).
func (s *Server) policyRefusal(w http.ResponseWriter, r *http.Request, form policyFormVM) {
	vm, err := s.policiesVMFor(r, &form, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, policiesList(vm))
}

// policiesVMFor builds the policies surface, optionally carrying the add
// dialog's refusal state and a whole-surface notice (nil/empty = fresh page).
func (s *Server) policiesVMFor(r *http.Request, form *policyFormVM, notice string) (policiesVM, error) {
	rows, err := s.policies(r)
	if err != nil {
		return policiesVM{}, err
	}
	ident, _ := identityOf(r.Context())
	vm := policiesVM{
		Page: s.pageFor(r, "Plan policies", "policies"), Policies: rows,
		Operator: canWrite(ident), Form: form, Notice: notice,
	}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "policies-add", Label: "policy"}
	}
	return vm, nil
}

func (s *Server) handlePolicyRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if !r.Form.Has("confirmed") {
		http.Error(w, "delete requires confirmation", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	if err := s.store.DeletePlanPolicy(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.log.Info("web: policy rm refused", "policy", name, "by", ident.token.Name)
			s.policyRmRefusal(w, r, name)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: policy rm", "policy", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/policies", "removed "+name)
}

// policyRmRefusal re-renders the policies surface at 404 with the refusal
// shown on it — a button-verb rm's failure renders on the surface, never a
// flash.
func (s *Server) policyRmRefusal(w http.ResponseWriter, r *http.Request, name string) {
	vm, err := s.policiesVMFor(r, nil, "No policy named "+name+" exists to remove.")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusNotFound, policiesList(vm))
}
