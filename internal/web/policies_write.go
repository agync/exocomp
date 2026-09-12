package web

import (
	"errors"
	"fmt"
	"net/http"

	"exocomp.dev/exocomp/internal/policy"
	"exocomp.dev/exocomp/internal/store"
)

// A plan policy carries no secret material — it is a name, a workspace, a CEL
// expression, and an action — so this lane is operator-scope only, like
// schedules: no reauth, no confirm_token. add mirrors `exocomp policy add`
// (cmd/exocomp/policy.go): the expression is compiled here so a policy that
// could never decide is refused now rather than silently failing a later seal.

func init() { registerWrite((*Server).registerPolicyWrite) }

func (s *Server) registerPolicyWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /policies/add", s.requireAuth(s.handlePolicyAdd))
	mux.HandleFunc("POST /policies/{name}/rm", s.requireAuth(s.handlePolicyRm))
}

func (s *Server) handlePolicyAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PostFormValue("name")
	workspace := r.PostFormValue("workspace")
	expr := r.PostFormValue("expr")
	action := r.PostFormValue("action")

	if workspace == "" {
		s.fail(w, r, errors.New("policy add: workspace is required"))
		return
	}
	switch action {
	case store.PolicyDeny, store.PolicyRequireApproval:
	default:
		s.fail(w, r, fmt.Errorf("policy add: action must be %s or %s",
			store.PolicyDeny, store.PolicyRequireApproval))
		return
	}
	// Refuse an expression that cannot compile or does not decide a bool, the
	// same check `exocomp policy add` runs — a policy that cannot decide must
	// never reach a seal.
	if _, err := policy.Compile(expr); err != nil {
		s.fail(w, r, fmt.Errorf("policy add: %w", err))
		return
	}

	created, err := s.store.CreatePlanPolicy(r.Context(), store.PlanPolicy{
		Name: name, Workspace: workspace, Expr: expr, Action: action,
	})
	if errors.Is(err, store.ErrDuplicate) {
		http.Error(w, "a policy named "+name+" already exists", http.StatusConflict)
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
			http.Error(w, "no policy named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: policy rm", "policy", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/policies", "removed "+name)
}
