package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"exocomp.dev/exocomp/internal/store"
)

// A netgroup names a set of CIDRs — the operator's own vocabulary for
// classifying an address, never a secret. So, like schedules, this lane is
// operator-scope only: no reauth, no confirm_token. The comma-split +
// ParsePrefix validation below reimplements cmd/exocomp/netgroup.go's
// parsePrefixList, so a group added from the browser is held to exactly the
// same rules as one added from the CLI.

func init() { registerWrite((*Server).registerNetgroupWrite) }

func (s *Server) registerNetgroupWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /netgroups/add", s.requireAuth(s.handleNetgroupAdd))
	mux.HandleFunc("POST /netgroups/{name}/rm", s.requireAuth(s.handleNetgroupRm))
}

// parsePrefixList splits a comma-separated CIDR field and parses every member.
// An empty member is refused too: in the middle of a hand-typed list it is a
// double comma, a typo that should be caught here rather than stored.
func parsePrefixList(spec string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("an empty CIDR (double comma) in the list")
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR: %w", part, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) handleNetgroupAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PostFormValue("name")
	prefixes, err := parsePrefixList(r.PostFormValue("prefixes"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	created, err := s.store.CreateNetgroup(r.Context(), store.NewNetgroup{
		Name: name, Prefixes: prefixes,
	})
	if errors.Is(err, store.ErrDuplicate) {
		http.Error(w, "a netgroup named "+name+" already exists", http.StatusConflict)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: netgroup add", "netgroup", created.Name, "by", ident.token.Name)
	redirectFlash(w, r, "/netgroups", "added "+created.Name)
}

func (s *Server) handleNetgroupRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
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
	redirectFlash(w, r, "/netgroups", "removed "+name)
}
