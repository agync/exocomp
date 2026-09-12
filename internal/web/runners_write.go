package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"exocomp.dev/exocomp/internal/store"
)

// A runner definition holds no secret material — a base image reference, an
// optional bootstrap script or Containerfile path, package pins, and the
// caps a run would ask for — so this lane is operator-scope only, the same
// as schedules: no reauth, no confirm_token. Registration is still inert: the
// container tier that would build and run the image is deferred (runner.go's
// deferredNote), and this lane stores exactly what `exocomp runner add`
// stores.

func init() { registerWrite((*Server).registerRunnerWrite) }

func (s *Server) registerRunnerWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /runners/add", s.requireAuth(s.handleRunnerAdd))
	mux.HandleFunc("POST /runners/{name}/rm", s.requireAuth(s.handleRunnerRm))
}

// runnerNameMax and guardRunnerName mirror cmd/exocomp/runner.go's checks: a
// runner name doubles as an image-tag component for the deferred build tier,
// so it is held to that shape here too rather than only at CLI entry.
const runnerNameMax = 64

func guardRunnerName(name string) error {
	if name == "" || len(name) > runnerNameMax {
		return fmt.Errorf("runner name %q must be 1-%d characters", name, runnerNameMax)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_' || r == '.') && i > 0:
		default:
			return fmt.Errorf("runner name %q must start with a lowercase letter or digit "+
				"and hold only [a-z0-9._-]", name)
		}
	}
	return nil
}

// validateRunner mirrors cmd/exocomp/runner.go's validateRunner, so a runner
// registered from the browser is held to exactly the same rules as one
// registered from the CLI.
func validateRunner(r store.Runner) error {
	if err := guardRunnerName(r.Name); err != nil {
		return err
	}
	if r.Mode != store.RunnerModeCached && r.Mode != store.RunnerModeOnTheFly {
		return fmt.Errorf("mode must be %s or %s, not %q",
			store.RunnerModeCached, store.RunnerModeOnTheFly, r.Mode)
	}
	if strings.TrimSpace(r.Base) == "" {
		return fmt.Errorf("runner %q needs a base image", r.Name)
	}
	if r.Bootstrap != "" && r.Containerfile != "" {
		return fmt.Errorf("runner %q gives both a bootstrap and a containerfile; "+
			"they are alternative build models, not a merge", r.Name)
	}
	return nil
}

func (s *Server) handleRunnerAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	mode := r.PostFormValue("mode")
	if mode == "" {
		mode = store.RunnerModeCached
	}
	var packages []string
	for _, p := range strings.Split(r.PostFormValue("packages"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			packages = append(packages, p)
		}
	}

	rn := store.Runner{
		Name:          r.PostFormValue("name"),
		Mode:          mode,
		Base:          r.PostFormValue("base"),
		Bootstrap:     r.PostFormValue("bootstrap"),
		Containerfile: r.PostFormValue("containerfile"),
		Packages:      packages,
		AnsibleCore:   r.PostFormValue("ansible_core"),
		MemoryMax:     r.PostFormValue("memory_max"),
		CPUQuota:      r.PostFormValue("cpu_quota"),
	}
	if err := validateRunner(rn); err != nil {
		s.fail(w, r, err)
		return
	}

	created, err := s.store.CreateRunner(r.Context(), rn)
	if errors.Is(err, store.ErrDuplicate) {
		http.Error(w, "a runner named "+rn.Name+" already exists", http.StatusConflict)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: runner add", "runner", created.Name, "mode", created.Mode, "by", ident.token.Name)
	s.redirectFlash(w, r, "/runners", "registered "+created.Name)
}

func (s *Server) handleRunnerRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	if !r.Form.Has("confirmed") {
		http.Error(w, "delete requires confirmation", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	if err := s.store.DeleteRunner(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no runner named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: runner rm", "runner", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/runners", "removed "+name)
}
