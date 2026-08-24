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
		s.fail(w, r, err)
		return
	}
	kind := r.PostFormValue("kind")
	args, err := formScheduleArgs(r, kind)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	concurrencyKey := r.PostFormValue("concurrency_key")
	if (kind == store.ScheduleBackup || kind == store.ScheduleDrill) && concurrencyKey != "" {
		s.fail(w, r, fmt.Errorf("kind %s keys each fire by target; concurrency key does not apply", kind))
		return
	}
	jitter, err := formJitter(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	created, err := s.store.CreateSchedule(r.Context(), store.NewSchedule{
		Name: name, CronExpr: cronExpr, Kind: kind, Args: args,
		ConcurrencyKey: concurrencyKey, JitterS: jitter,
	})
	if errors.Is(err, store.ErrDuplicate) {
		http.Error(w, "a schedule named "+name+" already exists", http.StatusConflict)
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
