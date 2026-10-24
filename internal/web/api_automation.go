package web

import (
	"net/http"
	"time"

	"exocomp.dev/exocomp/internal/sched"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// This file mounts the /api/v1 writes for schedules and rules: what `exocomp
// schedule` and `exocomp rule` do on the daemon's own host. They take the
// operator scope as a bearer, as the browser's schedule form takes an operator
// session, and validate through the same service verbs the CLI's local path
// calls. A running daemon picks a schedule change up on its next reload, as it
// does for one the CLI writes.

const (
	kindSchedule        = "schedule.get"
	kindScheduleRemoved = "schedule.removed"
	kindRule            = "rule.get"
	kindRuleRemoved     = "rule.removed"
)

func (s *Server) registerAutomationAPI(mux *http.ServeMux) {
	write := func(what string, h http.HandlerFunc) http.HandlerFunc {
		return s.apiGuard(bearerOnly(canWrite), writeDeny(what, "operator"), h)
	}
	mux.HandleFunc("POST /api/v1/schedules", write("manage schedules", s.apiScheduleAdd))
	mux.HandleFunc("DELETE /api/v1/schedules/{name}", write("manage schedules", s.apiScheduleRm))
	mux.HandleFunc("POST /api/v1/schedules/{name}/enable", write("manage schedules", s.apiScheduleSetEnabled(true)))
	mux.HandleFunc("POST /api/v1/schedules/{name}/disable", write("manage schedules", s.apiScheduleSetEnabled(false)))
	mux.HandleFunc("POST /api/v1/rules", write("manage rules", s.apiRuleAdd))
	mux.HandleFunc("DELETE /api/v1/rules/{name}", write("manage rules", s.apiRuleRm))
	mux.HandleFunc("POST /api/v1/rules/{name}/enable", write("manage rules", s.apiRuleSetEnabled(true)))
	mux.HandleFunc("POST /api/v1/rules/{name}/disable", write("manage rules", s.apiRuleSetEnabled(false)))
}

type nameView struct {
	Name string `json:"name"`
}

// --- schedules -------------------------------------------------------------

type scheduleAddRequest struct {
	Name                string   `json:"name"`
	Cron                string   `json:"cron"`
	Kind                string   `json:"kind"`
	Argv                []string `json:"argv"`
	Target              string   `json:"target"`
	Source              string   `json:"source"`
	TolerateUnreachable bool     `json:"tolerate_unreachable"`
	ConcurrencyKey      string   `json:"concurrency_key"`
	JitterS             int      `json:"jitter_s"`
}

func (s *Server) scheduleView(ctx *http.Request, name string) (wire.ScheduleView, error) {
	rows, err := s.store.ListSchedules(ctx.Context(), false)
	if err != nil {
		return wire.ScheduleView{}, err
	}
	for _, v := range wire.NewScheduleList(rows) {
		if v.Name == name {
			return v, nil
		}
	}
	return wire.ScheduleView{}, store.ErrNotFound
}

func (s *Server) apiScheduleAdd(w http.ResponseWriter, r *http.Request) {
	var req scheduleAddRequest
	if !s.decodeBody(w, r, &req) {
		return
	}
	n := store.NewSchedule{
		Name: req.Name, CronExpr: req.Cron, Kind: req.Kind,
		Args: store.ScheduleArgs{
			Argv: req.Argv, Target: req.Target, Source: req.Source, TolerateUnreachable: req.TolerateUnreachable,
		},
		ConcurrencyKey: req.ConcurrencyKey, JitterS: req.JitterS,
	}
	if err := sched.CheckNewSchedule(n); err != nil {
		s.adminFail(w, err)
		return
	}
	// A play's argv after the playbook reaches ansible-playbook as options, so
	// this face takes only the check-mode pair `schedule add --check` sends; a
	// key file, an extra-vars file or ssh options would sidestep what the daemon
	// grants a run.
	if n.Kind == sched.KindPlay {
		for _, a := range n.Args.Argv[1:] {
			if a != "--check" && a != "--diff" {
				s.badRequest(w, "a play schedule takes its playbook and --check/--diff over the API; "+
					a+" is set on the daemon's host with --local")
				return
			}
		}
	}
	created, err := s.service().CreateSchedule(r.Context(), n, actorFromRequest(r))
	if err != nil {
		s.adminFail(w, err)
		return
	}
	view, err := s.scheduleView(r, created.Name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.log.Info("api: schedule add", "schedule", created.Name, "kind", created.Kind, "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindSchedule, view)
}

func (s *Server) apiScheduleRm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.store.DeleteSchedule(r.Context(), name); err != nil {
		s.apiFail(w, err)
		return
	}
	s.log.Info("api: schedule rm", "schedule", name, "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindScheduleRemoved, nameView{Name: name})
}

func (s *Server) apiScheduleSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := s.store.SetScheduleEnabled(r.Context(), name, enabled); err != nil {
			s.apiFail(w, err)
			return
		}
		view, err := s.scheduleView(r, name)
		if err != nil {
			s.apiFail(w, err)
			return
		}
		s.log.Info("api: schedule set enabled", "schedule", name, "enabled", enabled, "by", actorFromRequest(r).Name)
		s.apiSuccess(w, kindSchedule, view)
	}
}

// --- rules -----------------------------------------------------------------

// ruleAddRequest is `rule add` as fields. It carries only what the CLI offers:
// allow_cross_host, which no face sets at add time, is not a field.
type ruleAddRequest struct {
	Name             string   `json:"name"`
	SourceFilter     string   `json:"source_filter"`
	Expr             string   `json:"expr"`
	Action           string   `json:"action"`
	Argv             []string `json:"argv"`
	Workspace        string   `json:"workspace"`
	Source           string   `json:"source"`
	Playbook         string   `json:"playbook"`
	Limit            string   `json:"limit"`
	Tags             []string `json:"tags"`
	Check            bool     `json:"check"`
	Diff             bool     `json:"diff"`
	Timeout          string   `json:"timeout"`
	Egress           []int    `json:"egress"`
	SSHIdentityFiles []string `json:"ssh_identity_files"`
	CooldownS        int      `json:"cooldown_s"`
	DailyCap         int      `json:"daily_cap"`
	RequireApproval  bool     `json:"require_approval"`
}

func (s *Server) ruleView(r *http.Request, name string) (wire.RuleView, error) {
	rows, err := s.store.ListRules(r.Context(), false)
	if err != nil {
		return wire.RuleView{}, err
	}
	for _, v := range wire.NewRuleList(rows) {
		if v.Name == name {
			return v, nil
		}
	}
	return wire.RuleView{}, store.ErrNotFound
}

func (s *Server) apiRuleAdd(w http.ResponseWriter, r *http.Request) {
	var req ruleAddRequest
	if !s.decodeBody(w, r, &req) {
		return
	}
	var timeout time.Duration
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err != nil {
			s.badRequest(w, "timeout is not a valid Go duration")
			return
		}
		timeout = d
	}
	// A caller on another host names identity paths on this one, so each must
	// be a file the daemon's operator allowed, as for a playbook run.
	identities, err := s.allowedIdentities(req.SSHIdentityFiles)
	if err != nil {
		s.badRequest(w, err.Error())
		return
	}
	created, err := s.service().CreateRule(r.Context(), store.NewRule{
		Name: req.Name, SourceFilter: req.SourceFilter, CELExpr: req.Expr, ActionKind: req.Action,
		Action: store.RuleAction{
			Argv: req.Argv, Workspace: req.Workspace, Source: req.Source, Playbook: req.Playbook,
			Limit: req.Limit, Tags: req.Tags, Check: req.Check, Diff: req.Diff,
			Timeout: store.Duration(timeout), Egress: req.Egress, SSHIdentityFiles: identities,
		},
		CooldownS: req.CooldownS, DailyCap: req.DailyCap, RequireApproval: req.RequireApproval,
	}, actorFromRequest(r))
	if err != nil {
		s.adminFail(w, err)
		return
	}
	view, err := s.ruleView(r, created.Name)
	if err != nil {
		s.apiFail(w, err)
		return
	}
	s.log.Info("api: rule add", "rule", created.Name, "action", created.ActionKind, "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindRule, view)
}

func (s *Server) apiRuleRm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.store.DeleteRule(r.Context(), name); err != nil {
		s.apiFail(w, err)
		return
	}
	s.log.Info("api: rule rm", "rule", name, "by", actorFromRequest(r).Name)
	s.apiSuccess(w, kindRuleRemoved, nameView{Name: name})
}

func (s *Server) apiRuleSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := s.store.SetRuleEnabled(r.Context(), name, enabled); err != nil {
			s.apiFail(w, err)
			return
		}
		view, err := s.ruleView(r, name)
		if err != nil {
			s.apiFail(w, err)
			return
		}
		s.log.Info("api: rule set enabled", "rule", name, "enabled", enabled, "by", actorFromRequest(r).Name)
		s.apiSuccess(w, kindRule, view)
	}
}
