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
// stores. Refusals re-render the dialog at 422 with every field's message at
// once (forms/SPEC.md); the raw validator words go to the log, never the
// operator.

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

// The add dialog's human refusal copy (forms/copy.txt): one sentence in the
// field's terms, never the validator's constraint or a status code.
const (
	runnerNameEmpty     = "Give the runner a name."
	runnerNameLength    = "Names are 64 characters or fewer."
	runnerNameCharset   = "Names start with a lowercase letter or digit and hold letters, digits, dots, hyphens and underscores."
	runnerModeRefusal   = "Mode is cached or onthefly."
	runnerBaseRequired  = "Give the base image a name, like registry.fedoraproject.org/fedora-minimal:44."
	runnerBothRefusal   = "Give a bootstrap script or a Containerfile path — not both."
	runnerSubmitRefusal = "Couldn't register the runner — nothing was saved."
)

func runnerNameRefusal(name string, err error) (msg, fix string) {
	raw := err.Error()
	if strings.TrimSpace(name) == "" {
		return runnerNameEmpty, ""
	}
	if strings.Contains(raw, "must be 1-") {
		return runnerNameLength, netgroupSlug(name)
	}
	if strings.Contains(name, " ") {
		return "Names can't have spaces.", netgroupSlug(name)
	}
	return runnerNameCharset, netgroupSlug(name)
}

func runnerDuplicateRefusal(name string) string {
	return "A runner named " + name + " already exists."
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
	form := runnerFormVM{
		Name: r.PostFormValue("name"), Mode: mode, Base: r.PostFormValue("base"),
		Bootstrap: r.PostFormValue("bootstrap"), Containerfile: r.PostFormValue("containerfile"),
		Packages: r.PostFormValue("packages"), AnsibleCore: r.PostFormValue("ansible_core"),
		MemoryMax: r.PostFormValue("memory_max"), CPUQuota: r.PostFormValue("cpu_quota"),
	}

	// Validate every field up front so all refusals render at once (forms/
	// SPEC.md); the daemon stays authoritative on uniqueness.
	var causes []string
	if err := guardRunnerName(form.Name); err != nil {
		form.ErrName, form.FixName = runnerNameRefusal(form.Name, err)
		causes = append(causes, err.Error())
	}
	switch form.Mode {
	case store.RunnerModeCached, store.RunnerModeOnTheFly:
	default:
		form.ErrMode = runnerModeRefusal
		causes = append(causes, "mode "+form.Mode)
	}
	if strings.TrimSpace(form.Base) == "" {
		form.ErrBase = runnerBaseRequired
		causes = append(causes, "empty base")
	}
	if form.Bootstrap != "" && form.Containerfile != "" {
		form.ErrContainerfile = runnerBothRefusal
		causes = append(causes, "bootstrap and containerfile")
	}

	if form.ErrName != "" || form.ErrMode != "" || form.ErrBase != "" || form.ErrContainerfile != "" {
		switch {
		case form.ErrName != "":
			form.FirstInvalid = "name"
		case form.ErrMode != "":
			form.FirstInvalid = "mode"
		case form.ErrBase != "":
			form.FirstInvalid = "base"
		case form.ErrContainerfile != "":
			form.FirstInvalid = "containerfile"
		}
		s.log.Info("web: runner add refused", "err", strings.Join(causes, "; "), "by", ident.token.Name)
		s.runnerRefusal(w, r, form)
		return
	}

	var packages []string
	for _, p := range strings.Split(form.Packages, ",") {
		if p = strings.TrimSpace(p); p != "" {
			packages = append(packages, p)
		}
	}

	rn := store.Runner{
		Name: form.Name, Mode: form.Mode, Base: form.Base,
		Bootstrap: form.Bootstrap, Containerfile: form.Containerfile,
		Packages: packages, AnsibleCore: form.AnsibleCore,
		MemoryMax: form.MemoryMax, CPUQuota: form.CPUQuota,
	}
	created, err := s.store.CreateRunner(r.Context(), rn)
	if errors.Is(err, store.ErrDuplicate) {
		s.log.Info("web: runner add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrName = runnerDuplicateRefusal(form.Name)
		form.FirstInvalid = "name"
		s.runnerRefusal(w, r, form)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: runner add", "runner", created.Name, "mode", created.Mode, "by", ident.token.Name)
	s.redirectFlash(w, r, "/runners", "registered "+created.Name)
}

// runnerRefusal re-renders the runners surface at 422 with the add dialog
// carrying its refusals — the JS-off path, and the fragment body a JS submit
// swaps in place. Refusals never flash: flash is success-only (write.go).
func (s *Server) runnerRefusal(w http.ResponseWriter, r *http.Request, form runnerFormVM) {
	vm, err := s.runnersVMFor(r, &form, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, runnersList(vm))
}

// runnersVMFor builds the runners surface, optionally carrying the add
// dialog's refusal state and a whole-surface notice (nil/empty = fresh page).
func (s *Server) runnersVMFor(r *http.Request, form *runnerFormVM, notice string) (runnersVM, error) {
	rows, err := s.runners(r)
	if err != nil {
		return runnersVM{}, err
	}
	ident, _ := identityOf(r.Context())
	vm := runnersVM{
		Page: s.pageFor(r, "Runners", "runners"), Runners: rows,
		Operator: canWrite(ident), Form: form, Notice: notice,
	}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "runners-add", Label: "runner"}
	}
	return vm, nil
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
			s.log.Info("web: runner rm refused", "runner", name, "by", ident.token.Name)
			s.runnerRmRefusal(w, r, name)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: runner rm", "runner", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/runners", "removed "+name)
}

// runnerRmRefusal re-renders the runners surface at 404 with the refusal
// shown on it — a button-verb rm's failure renders on the surface, never a
// flash.
func (s *Server) runnerRmRefusal(w http.ResponseWriter, r *http.Request, name string) {
	vm, err := s.runnersVMFor(r, nil, "No runner named "+name+" exists to remove.")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusNotFound, runnersList(vm))
}
