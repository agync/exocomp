package web

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// A check names a probe set — targets and an interval — never a secret. So,
// like schedules, this lane is operator-scope only: no reauth, no
// confirm_token. The name=spec parsing below reimplements
// cmd/exocomp/check.go's parseCheckTargets, so a check added from the browser
// is held to exactly the same rules as one added from the CLI.

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
		s.fail(w, r, fmt.Errorf("interval must be a Go duration (e.g. 5m): %w", err))
		return
	}
	if interval <= 0 {
		s.fail(w, r, errors.New("interval must be a positive Go duration (e.g. 5m)"))
		return
	}
	targets, err := formCheckTargets(r, kind)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(targets) == 0 {
		s.fail(w, r, errors.New("need at least one target"))
		return
	}
	expect, err := formExpect(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if expect != 0 {
		if kind != store.CheckHTTP {
			s.fail(w, r, errors.New("expected status applies to kind http"))
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
		http.Error(w, "a check named "+name+" already exists", http.StatusConflict)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: check add", "check", created.Name, "kind", created.Kind, "by", ident.token.Name)
	redirectFlash(w, r, "/checks", "added "+created.Name)
}

// formCheckTargets splits the repeated name=spec target fields by kind, the
// same way cmd/exocomp/check.go's parseCheckTargets splits --target flags: the
// spec after the '=' is an IP literal (tcp/ping) or a URL (http); a tcp target
// carries its port after the last ':'. A blank field — an unused row of the
// form — is skipped, not an error.
func formCheckTargets(r *http.Request, kind string) ([]store.CheckTarget, error) {
	var out []store.CheckTarget
	for _, spec := range r.PostForm["target"] {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		name, value, ok := strings.Cut(spec, "=")
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("target %q must be name=spec", spec)
		}
		t := store.CheckTarget{Name: name}
		switch kind {
		case store.CheckTCP:
			host, port, err := net.SplitHostPort(value)
			if err != nil {
				return nil, fmt.Errorf("target %q: a tcp target must be ip:port", spec)
			}
			p, err := strconv.Atoi(port)
			if err != nil {
				return nil, fmt.Errorf("target %q: port %q is not a number", spec, port)
			}
			t.Address = host
			t.Port = p
		case store.CheckPing:
			t.Address = value
		case store.CheckHTTP:
			t.URL = value
		default:
			return nil, fmt.Errorf("kind must be %s, %s or %s",
				store.CheckTCP, store.CheckPing, store.CheckHTTP)
		}
		out = append(out, t)
	}
	return out, nil
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
	redirectFlash(w, r, "/checks", verb+" "+name)
}

func (s *Server) handleCheckRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
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
	redirectFlash(w, r, "/checks", "removed "+name)
}
