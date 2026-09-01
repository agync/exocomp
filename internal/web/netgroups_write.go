package web

import (
	"errors"
	"net/http"

	"exocomp.dev/exocomp/internal/store"
)

// A netgroup names a set of CIDRs — the operator's own vocabulary for
// classifying an address, never a secret. So, like schedules, this lane is
// operator-scope only: no reauth, no confirm_token. The prefixes field is
// parsed by the shared store.ParsePrefixList, so a group added from the
// browser is held to exactly the same rules as one added from the CLI.

func init() { registerWrite((*Server).registerNetgroupWrite) }

func (s *Server) registerNetgroupWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /netgroups/add", s.requireAuth(s.handleNetgroupAdd))
	mux.HandleFunc("POST /netgroups/{name}/rm", s.requireAuth(s.handleNetgroupRm))
}

func (s *Server) handleNetgroupAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PostFormValue("name")
	prefixes, err := store.ParsePrefixList(r.PostFormValue("prefixes"))
	if err != nil {
		redirectFlash(w, r, "/checks?tab=netgroups", err.Error())
		return
	}
	created, err := s.store.CreateNetgroup(r.Context(), store.NewNetgroup{
		Name: name, Prefixes: prefixes,
	})
	if errors.Is(err, store.ErrDuplicate) {
		redirectFlash(w, r, "/checks?tab=netgroups", "a netgroup named "+name+" already exists")
		return
	}
	if errors.Is(err, store.ErrValidation) {
		redirectFlash(w, r, "/checks?tab=netgroups", validationFlash(err))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: netgroup add", "netgroup", created.Name, "by", ident.token.Name)
	redirectFlash(w, r, "/checks?tab=netgroups", "added "+created.Name)
}

func (s *Server) handleNetgroupRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if !r.Form.Has("confirmed") {
		http.Error(w, "delete requires confirmation", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	if err := s.store.DeleteNetgroup(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no netgroup named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: netgroup rm", "netgroup", name, "by", ident.token.Name)
	redirectFlash(w, r, "/checks?tab=netgroups", "removed "+name)
}
