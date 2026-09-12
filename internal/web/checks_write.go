package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// A check names a probe set — targets and an interval — never a secret. So,
// like schedules, this lane is operator-scope only: no reauth, no
// confirm_token. The name=spec targets are parsed by the shared
// store.ParseCheckTargets (blank-skipping for the form's empty rows), so a
// check added from the browser is held to exactly the same per-kind rules as
// one added from the CLI.

func init() { registerWrite((*Server).registerCheckWrite) }

func (s *Server) registerCheckWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /checks/add", s.requireAuth(s.handleCheckAdd))
	mux.HandleFunc("POST /checks/{name}/enable", s.requireAuth(s.handleCheckEnable))
	mux.HandleFunc("POST /checks/{name}/disable", s.requireAuth(s.handleCheckDisable))
	mux.HandleFunc("POST /checks/{name}/rm", s.requireAuth(s.handleCheckRm))
}

func (s *Server) handleCheckAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PostFormValue("name")
	kind := r.PostFormValue("kind")
	interval, err := time.ParseDuration(r.PostFormValue("interval"))
	if err != nil {
		s.redirectFlash(w, r, "/checks", fmt.Sprintf("interval must be a Go duration (e.g. 5m): %v", err))
		return
	}
	if interval <= 0 {
		s.redirectFlash(w, r, "/checks", "interval must be a positive Go duration (e.g. 5m)")
		return
	}
	switch kind {
	case store.CheckTCP, store.CheckPing, store.CheckHTTP:
	default:
		s.redirectFlash(w, r, "/checks", fmt.Sprintf("kind must be %s, %s or %s", store.CheckTCP, store.CheckPing, store.CheckHTTP))
		return
	}
	targets, err := store.ParseCheckTargets(kind, r.PostForm["target"], true)
	if err != nil {
		s.redirectFlash(w, r, "/checks", err.Error())
		return
	}
	if len(targets) == 0 {
		s.redirectFlash(w, r, "/checks", "need at least one target")
		return
	}
	expect, err := formExpect(r)
	if err != nil {
		s.redirectFlash(w, r, "/checks", err.Error())
		return
	}
	if expect != 0 {
		if kind != store.CheckHTTP {
			s.redirectFlash(w, r, "/checks", "expected status applies to kind http")
			return
		}
		for i := range targets {
			targets[i].Expect = expect
		}
	}

	created, err := s.store.CreateCheck(r.Context(), store.NewCheck{
		Name: name, Kind: kind, IntervalS: int64(interval.Seconds()), Targets: targets,
	})
	if errors.Is(err, store.ErrDuplicate) {
		s.redirectFlash(w, r, "/checks", "a check named "+name+" already exists")
		return
	}
	if errors.Is(err, store.ErrValidation) {
		s.redirectFlash(w, r, "/checks", validationFlash(err))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: check add", "check", created.Name, "kind", created.Kind, "by", ident.token.Name)
	s.redirectFlash(w, r, "/checks", "added "+created.Name)
}

// formExpect parses the optional expected-status field; blank means no pin.
func formExpect(r *http.Request) (int, error) {
	v := r.PostFormValue("expect")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New("expected status must be a whole number")
	}
	return n, nil
}

func (s *Server) handleCheckEnable(w http.ResponseWriter, r *http.Request) {
	s.handleCheckSetEnabled(w, r, true)
}

func (s *Server) handleCheckDisable(w http.ResponseWriter, r *http.Request) {
	s.handleCheckSetEnabled(w, r, false)
}

func (s *Server) handleCheckSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := s.store.SetCheckEnabled(r.Context(), name, enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no check named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	s.log.Info("web: check "+verb, "check", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/checks", verb+" "+name)
}

func (s *Server) handleCheckRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if !r.Form.Has("confirmed") {
		http.Error(w, "delete requires confirmation", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	if err := s.store.DeleteCheck(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no check named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: check rm", "check", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/checks", "removed "+name)
}
