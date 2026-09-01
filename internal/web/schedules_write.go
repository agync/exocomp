package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/store"
)

// Schedules carry no secret material, so this lane is operator-scope only —
// unlike tokens and secrets, nothing here is gated behind reauth. The field
// shape mirrors `exocomp schedule add` (cmd/exocomp/schedule.go): kind decides
// which of argv/target/check/tolerate-unreachable apply, and CreateSchedule's
// own unique-name and ValidateCron checks are the same ones the CLI relies on.

func init() { registerWrite((*Server).registerScheduleWrite) }

func (s *Server) registerScheduleWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /schedules/add", s.requireAuth(s.handleScheduleAdd))
	mux.HandleFunc("POST /schedules/{name}/enable", s.requireAuth(s.handleScheduleEnable))
	mux.HandleFunc("POST /schedules/{name}/disable", s.requireAuth(s.handleScheduleDisable))
	mux.HandleFunc("POST /schedules/{name}/rm", s.requireAuth(s.handleScheduleRm))
}

func (s *Server) handleScheduleAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PostFormValue("name")
	cronExpr := r.PostFormValue("cron")
	if err := sched.ValidateCron(cronExpr); err != nil {
		redirectFlash(w, r, "/schedules", err.Error())
		return
	}
	kind := r.PostFormValue("kind")
	args, err := formScheduleArgs(r, kind)
	if err != nil {
		redirectFlash(w, r, "/schedules", err.Error())
		return
	}
	concurrencyKey := r.PostFormValue("concurrency_key")
	if (kind == store.ScheduleBackup || kind == store.ScheduleDrill) && concurrencyKey != "" {
		redirectFlash(w, r, "/schedules", fmt.Sprintf("kind %s keys each fire by target; concurrency key does not apply", kind))
		return
	}
	jitter, err := formJitter(r)
	if err != nil {
		redirectFlash(w, r, "/schedules", err.Error())
		return
	}

	created, err := s.store.CreateSchedule(r.Context(), store.NewSchedule{
		Name: name, CronExpr: cronExpr, Kind: kind, Args: args,
		ConcurrencyKey: concurrencyKey, JitterS: jitter,
	})
	if errors.Is(err, store.ErrDuplicate) {
		redirectFlash(w, r, "/schedules", "a schedule named "+name+" already exists")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: schedule add", "schedule", created.Name, "kind", created.Kind, "by", ident.token.Name)
	redirectFlash(w, r, "/schedules", "added "+created.Name)
}

func (s *Server) handleScheduleEnable(w http.ResponseWriter, r *http.Request) {
	s.handleScheduleSetEnabled(w, r, true)
}

func (s *Server) handleScheduleDisable(w http.ResponseWriter, r *http.Request) {
	s.handleScheduleSetEnabled(w, r, false)
}

func (s *Server) handleScheduleSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := s.store.SetScheduleEnabled(r.Context(), name, enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no schedule named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	s.log.Info("web: schedule "+verb, "schedule", name, "by", ident.token.Name)
	redirectFlash(w, r, "/schedules", verb+" "+name)
}

func (s *Server) handleScheduleRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := s.store.DeleteSchedule(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no schedule named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: schedule rm", "schedule", name, "by", ident.token.Name)
	redirectFlash(w, r, "/schedules", "removed "+name)
}

// formScheduleArgs shapes ScheduleArgs from the add form the same way
// cmd/exocomp/schedule.go's RunE shapes them from flags: kind decides which
// fields apply, and a kind that needs a command reads it from a single
// whitespace-split "argv" field (no shell-quoting support — a command needing
// quoted arguments still wants the CLI).
func formScheduleArgs(r *http.Request, kind string) (store.ScheduleArgs, error) {
	var args store.ScheduleArgs
	argv := strings.Fields(r.PostFormValue("argv"))
	target := r.PostFormValue("target")

	switch kind {
	case store.ScheduleShell:
		if len(argv) == 0 {
			return args, errors.New("kind shell needs a command")
		}
		args.Argv = argv
	case store.ScheduleBackup:
		if len(argv) != 0 {
			return args, errors.New("kind backup takes no command")
		}
		args.Target = target
	case sched.KindPlay:
		if len(argv) == 0 {
			return args, errors.New("kind play needs a playbook command")
		}
		if target == "" {
			return args, errors.New("kind play needs a target inventory")
		}
		args.Argv = argv
		args.Target = target
		args.TolerateUnreachable = r.Form.Has("tolerate_unreachable")
		if r.Form.Has("check") {
			// The config-drift schedule: --check --diff after the playbook, the
			// same passthrough shape newScheduleAddCmd appends them in.
			args.Argv = append(args.Argv, "--check", "--diff")
		}
	case store.ScheduleDrill:
		if len(argv) != 0 {
			return args, errors.New("kind drill takes no command")
		}
		args.Target = target
	default:
		return args, fmt.Errorf("kind must be %s, %s, %s or %s",
			store.ScheduleShell, store.ScheduleBackup, sched.KindPlay, store.ScheduleDrill)
	}
	return args, nil
}

// schedulePrefill carries optional add-form field values for a deep link into
// the schedules surface, so "Schedule…" elsewhere in the UI can land here with
// the fields already filled instead of the operator retyping a command. Open
// reports whether any prefill was given, so the surface auto-opens the add
// modal instead of leaving the operator to notice the "+" button.
type schedulePrefill struct {
	Name   string
	Kind   string
	Argv   string
	Target string
	Open   bool
}

// schedulePrefillFor builds a schedulePrefill from a GET request's query
// params. ?from_job=<id> derives Kind/Argv from that job's own stored
// Kind/Argv the same way handleJobSchedule does (a form convenience only —
// the real validation is on POST /schedules/add, exactly as if the operator
// had typed the fields by hand); an id that does not resolve to a schedulable
// job is ignored rather than failing the page. Explicit ?name=&kind=&argv=&
// target= set the corresponding fields directly and win over any from_job
// derivation; ?runbook= is a fallback name suggestion when ?name= is absent.
func (s *Server) schedulePrefillFor(r *http.Request) schedulePrefill {
	q := r.URL.Query()
	var p schedulePrefill

	if idStr := q.Get("from_job"); idStr != "" {
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
			if job, err := s.store.GetJob(r.Context(), id); err == nil && canScheduleJob(job) {
				p.Kind = store.ScheduleShell
				p.Argv = strings.Join(job.Args.Argv, " ")
				p.Open = true
			}
		}
	}
	if v := q.Get("runbook"); v != "" {
		p.Name = v
		p.Open = true
	}
	if v := q.Get("name"); v != "" {
		p.Name = v
		p.Open = true
	}
	if v := q.Get("kind"); v != "" {
		p.Kind = v
		p.Open = true
	}
	if v := q.Get("argv"); v != "" {
		p.Argv = v
		p.Open = true
	}
	if v := q.Get("target"); v != "" {
		p.Target = v
		p.Open = true
	}
	return p
}

// formJitter parses the optional jitter-seconds field.
func formJitter(r *http.Request) (int, error) {
	v := r.PostFormValue("jitter")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New("jitter must be a whole number of seconds")
	}
	if n < 0 {
		return 0, errors.New("jitter cannot be negative")
	}
	return n, nil
}
