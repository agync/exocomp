package web

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
)

// The run surface is the browser form face of the same enqueue verbs
// api_write.go's apiPlaybookRun (playbook) and internal/service.RunAdhoc
// (command) call, so a browser operator and an API/CLI caller reach the queue
// through the SAME path (AGENTS.md invariant 5). Unlike tokens/secrets, a run
// is not reauth-gated: it mints no credential and seals nothing, so the
// operator scope check in writeGuard is the whole gate. --require-approval on
// the command form routes the job through the existing approval gate instead
// of running it.

func init() { registerWrite((*Server).registerRunWrite) }

func (s *Server) registerRunWrite(mux *http.ServeMux) {
	mux.HandleFunc("GET /run", s.requireAuth(s.handleRunForm))
	mux.HandleFunc("POST /run/playbook", s.requireAuth(s.handleRunPlaybook))
	mux.HandleFunc("POST /run/command", s.requireAuth(s.handleRunCommand))
}

type runVM struct {
	Page     page
	Operator bool
}

// routeCommand namespaces an ad-hoc command run's idempotency key, alongside
// the api_write.go route labels (routePlan/routeApply/routePlaybook).
const routeCommand = "command.run"

func (s *Server) handleRunForm(w http.ResponseWriter, r *http.Request) {
	ident, _ := identityOf(r.Context())
	vm := runVM{Page: s.pageFor(r, "Run", "run"), Operator: canWrite(ident)}
	s.render(w, r, http.StatusOK, runPage(vm))
}

// handleRunPlaybook mirrors apiPlaybookRun (api_write.go): the same
// service.ResolveSourcePlaybook/ResolveInventory resolution, the same
// service.RunPlaybook call, differing only in the Enqueuer (this listener's
// pure-enqueue one) and in rendering a redirect instead of a JSON envelope.
func (s *Server) handleRunPlaybook(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}

	playbook := r.PostFormValue("playbook")
	if playbook == "" {
		http.Error(w, "playbook is required", http.StatusBadRequest)
		return
	}
	timeout, err := parseOptionalDuration(r, "timeout")
	if err != nil {
		http.Error(w, "timeout: "+err.Error(), http.StatusBadRequest)
		return
	}

	passthrough, vars, err := service.AnsiblePassthrough(
		r.PostFormValue("limit"), r.PostFormValue("tags"), "",
		r.Form.Has("check"), r.Form.Has("diff"), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	svc := s.service()
	if src := r.PostFormValue("source"); src != "" {
		if err := service.ValidSourceName(src); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		override := filepath.Join(s.dataDir, "hooks", src+".yml")
		hooked, abs, serr := svc.ResolveSourcePlaybook(r.Context(), override, src, playbook)
		if errors.Is(serr, service.ErrSourcePathEscape) {
			http.Error(w, serr.Error(), http.StatusBadRequest)
			return
		}
		if serr != nil {
			s.fail(w, r, serr)
			return
		}
		if hooked {
			http.Error(w, "source "+src+" declares hooks; its lifecycle workflow is not offered on this surface yet",
				http.StatusNotImplemented)
			return
		}
		playbook = abs
	}
	playbook, err = filepath.Abs(playbook)
	if err != nil {
		http.Error(w, "playbook path cannot be resolved", http.StatusBadRequest)
		return
	}

	invPath, invName, err := svc.ResolveInventory(r.Context(), r.PostFormValue("inventory"))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	res, err := svc.RunPlaybook(r.Context(), s.enqueuer(), service.PlaybookInput{
		PlaybookPath:   playbook,
		InventoryPath:  invPath,
		InventoryName:  invName,
		RunSecrets:     r.PostFormValue("run_secrets"),
		Passthrough:    passthrough,
		ExtraVars:      vars,
		IdempotencyKey: idempotencyKey(r, routePlaybook),
		Timeout:        timeout,
		Actor:          service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name},
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: run playbook", "job", res.Job.ID, "playbook", playbook, "by", ident.token.Name)
	redirectFlash(w, r, "/jobs/"+strconv.FormatInt(res.Job.ID, 10), "job "+strconv.FormatInt(res.Job.ID, 10)+" enqueued")
}

// handleRunCommand parses the everyday ad-hoc form (host, command, shell,
// source, inventory, become, run_secrets, require_approval, timeout) and hands
// the resolved input to service.RunAdhoc, which assembles the queue.Spec and
// either enqueues it plainly or routes it through the approval gate.
func (s *Server) handleRunCommand(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}

	host := r.PostFormValue("host")
	command := r.PostFormValue("command")
	if command == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	module := "command"
	if r.Form.Has("shell") {
		module = "shell"
	}
	timeout, err := parseOptionalDuration(r, "timeout")
	if err != nil {
		http.Error(w, "timeout: "+err.Error(), http.StatusBadRequest)
		return
	}

	dir, dirReadOnly, err := s.resolveSourceCheckout(r.Context(), r.PostFormValue("source"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if errors.Is(err, service.ErrSourcePathEscape) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.fail(w, r, err)
		return
	}

	svc := s.service()
	invPath, invName, err := svc.ResolveInventory(r.Context(), r.PostFormValue("inventory"))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	res, err := svc.RunAdhoc(r.Context(), s.enqueuer(), s.deriver, service.AdhocInput{
		Runbook:         "command",
		Pattern:         host,
		Module:          module,
		Args:            command,
		Become:          r.Form.Has("become"),
		Dir:             dir,
		DirReadOnly:     dirReadOnly,
		InventoryPath:   invPath,
		InventoryName:   invName,
		RunSecrets:      r.PostFormValue("run_secrets"),
		RequireApproval: r.Form.Has("require_approval"),
		IdempotencyKey:  idempotencyKey(r, routeCommand),
		Timeout:         timeout,
		Actor:           service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name},
	})
	if err != nil {
		if errors.Is(err, service.ErrSourcePathEscape) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A guard refusal (bad host pattern/module/args) is a bad request, not a
		// server fault; RunAdhoc returns those as plain errors with no store
		// sentinel to match on, so a validation-shaped message is rendered 400.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if res.Gated {
		a := res.Approval
		s.log.Info("web: run command gated", "job", a.JobID, "approval", a.ApprovalID, "by", ident.token.Name)
		redirectFlash(w, r, "/approvals",
			"job "+strconv.FormatInt(a.JobID, 10)+" gated; approval "+strconv.FormatInt(a.ApprovalID, 10)+" pending")
		return
	}
	s.log.Info("web: run command", "job", res.Run.Job.ID, "host", host, "by", ident.token.Name)
	redirectFlash(w, r, "/jobs/"+strconv.FormatInt(res.Run.Job.ID, 10),
		"job "+strconv.FormatInt(res.Run.Job.ID, 10)+" enqueued")
}

// resolveSourceCheckout resolves a registered source's checkout root and whether
// a run may write to it, the web mirror of cmd/exocomp/adhoc.go's
// sourceCheckoutRoot: an empty name runs in the caller's own directory
// (no --source), which is writable.
func (s *Server) resolveSourceCheckout(ctx context.Context, name string) (root string, readOnly bool, err error) {
	if name == "" {
		return "", false, nil
	}
	if err := service.ValidSourceName(name); err != nil {
		return "", false, err
	}
	src, err := s.store.SourceByName(ctx, name)
	if err != nil {
		return "", false, err
	}
	loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return "", false, err
	}
	return loc.Root, loc.ReadOnly, nil
}

// parseOptionalDuration parses a form field as a Go duration; an empty value
// means no limit.
func parseOptionalDuration(r *http.Request, field string) (time.Duration, error) {
	v := r.PostFormValue(field)
	if v == "" {
		return 0, nil
	}
	return time.ParseDuration(v)
}
