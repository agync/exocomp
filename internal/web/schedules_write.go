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
// The one exception is --source: a play schedule that runs from a source is set
// from the CLI only; this form never writes one.

func init() { registerWrite((*Server).registerScheduleWrite) }

func (s *Server) registerScheduleWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /schedules/add", s.requireAuth(s.handleScheduleAdd))
	mux.HandleFunc("POST /schedules/{name}/run", s.requireAuth(s.handleScheduleRun))
	mux.HandleFunc("POST /schedules/{name}/enable", s.requireAuth(s.handleScheduleEnable))
	mux.HandleFunc("POST /schedules/{name}/disable", s.requireAuth(s.handleScheduleDisable))
	mux.HandleFunc("POST /schedules/{name}/rm", s.requireAuth(s.handleScheduleRm))
}

// The add dialog's human refusal copy (forms/copy.txt): one sentence in the
// field's terms, never the validator's words or a status code. The raw causes
// go to the log at each call site below.
const (
	scheduleCronRefusal        = "Give a 5-field cron or an @every — like 0 */5 * * * * or @every 5m."
	scheduleKindRefusal        = "Kind is shell, backup, play or drill."
	scheduleArgvRequired       = "Give a command — shell and play kinds need one."
	scheduleArgvForbidden      = "backup and drill kinds take no command — leave it empty."
	scheduleTargetRequired     = "Play kind needs a target inventory name."
	scheduleConcurrencyRefusal = "backup and drill kinds key each fire by target — the concurrency key does not apply."
	scheduleJitterRefusal      = "The jitter is a whole number of seconds — like 300."
	scheduleJitterNegative     = "The jitter can't be negative."
	scheduleSubmitRefusal      = "Couldn't add the schedule — nothing was saved."
	// scheduleRemoveConfirmRefusal is the rm verb's confirm-gate refusal
	// (forms/SPEC.md): the shared confirm dialog's hidden confirmed=1 is the
	// safety, and a confirmed-less remove is refused on the row.
	scheduleRemoveConfirmRefusal = "Nothing was removed — confirm the removal in the dialog."
)

func scheduleNameRefusal(name string) (msg, fix string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Give the schedule a name.", ""
	}
	if strings.Contains(name, " ") {
		msg = "Names can't have spaces."
	} else {
		msg = "Names use letters, digits, dots, hyphens and underscores."
	}
	return msg, netgroupSlug(name)
}

func scheduleDuplicateRefusal(name string) string {
	return "A schedule named " + name + " already exists."
}

func (s *Server) handleScheduleAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	form := scheduleFormVM{
		Name: r.PostFormValue("name"), Cron: r.PostFormValue("cron"),
		Kind: r.PostFormValue("kind"), Argv: r.PostFormValue("argv"),
		Target: r.PostFormValue("target"), ConcurrencyKey: r.PostFormValue("concurrency_key"),
		Jitter: r.PostFormValue("jitter"),
		Check:  r.Form.Has("check"), TolerateUnreachable: r.Form.Has("tolerate_unreachable"),
	}

	// Validate every field up front so all refusals render at once (forms/
	// SPEC.md); the daemon stays authoritative on uniqueness.
	var causes []string
	if !store.ValidName(form.Name) {
		form.ErrName, form.FixName = scheduleNameRefusal(form.Name)
		causes = append(causes, "name "+form.Name)
	}
	if err := sched.ValidateCron(form.Cron); err != nil {
		form.ErrCron = scheduleCronRefusal
		causes = append(causes, err.Error())
	}
	switch form.Kind {
	case store.ScheduleShell, store.ScheduleBackup, sched.KindPlay, store.ScheduleDrill:
	default:
		form.ErrKind = scheduleKindRefusal
		causes = append(causes, "kind "+form.Kind)
	}
	argv := strings.Fields(form.Argv)
	switch form.Kind {
	case store.ScheduleShell:
		if len(argv) == 0 {
			form.ErrArgv = scheduleArgvRequired
			causes = append(causes, "shell needs a command")
		}
	case store.ScheduleBackup, store.ScheduleDrill:
		if len(argv) != 0 {
			form.ErrArgv = scheduleArgvForbidden
			causes = append(causes, "argv on "+form.Kind)
		}
	case sched.KindPlay:
		if len(argv) == 0 {
			form.ErrArgv = scheduleArgvRequired
			causes = append(causes, "play needs a command")
		}
		if form.Target == "" {
			form.ErrTarget = scheduleTargetRequired
			causes = append(causes, "play needs a target")
		}
	}
	if (form.Kind == store.ScheduleBackup || form.Kind == store.ScheduleDrill) && form.ConcurrencyKey != "" {
		form.ErrConcurrency = scheduleConcurrencyRefusal
		causes = append(causes, "concurrency key on "+form.Kind)
	}
	if _, err := formJitter(r); err != nil {
		if strings.HasPrefix(strings.TrimSpace(form.Jitter), "-") {
			form.ErrJitter = scheduleJitterNegative
		} else {
			form.ErrJitter = scheduleJitterRefusal
		}
		causes = append(causes, "jitter "+form.Jitter)
	}

	if form.ErrName != "" || form.ErrCron != "" || form.ErrKind != "" ||
		form.ErrArgv != "" || form.ErrTarget != "" || form.ErrConcurrency != "" ||
		form.ErrJitter != "" {
		switch {
		case form.ErrName != "":
			form.FirstInvalid = "name"
		case form.ErrCron != "":
			form.FirstInvalid = "cron"
		case form.ErrKind != "":
			form.FirstInvalid = "kind"
		case form.ErrArgv != "":
			form.FirstInvalid = "argv"
		case form.ErrTarget != "":
			form.FirstInvalid = "target"
		case form.ErrConcurrency != "":
			form.FirstInvalid = "concurrency"
		case form.ErrJitter != "":
			form.FirstInvalid = "jitter"
		}
		s.log.Info("web: schedule add refused", "err", strings.Join(causes, "; "), "by", ident.token.Name)
		s.scheduleRefusal(w, r, form)
		return
	}

	args, err := formScheduleArgs(r, form.Kind)
	if err != nil {
		s.log.Info("web: schedule add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrForm = scheduleSubmitRefusal
		s.scheduleRefusal(w, r, form)
		return
	}
	jitter, err := formJitter(r)
	if err != nil {
		s.log.Info("web: schedule add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrForm = scheduleSubmitRefusal
		s.scheduleRefusal(w, r, form)
		return
	}

	created, err := s.store.CreateSchedule(r.Context(), store.NewSchedule{
		Name: form.Name, CronExpr: form.Cron, Kind: form.Kind, Args: args,
		ConcurrencyKey: form.ConcurrencyKey, JitterS: jitter,
	})
	if errors.Is(err, store.ErrDuplicate) {
		s.log.Info("web: schedule add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrName = scheduleDuplicateRefusal(form.Name)
		form.FirstInvalid = "name"
		s.scheduleRefusal(w, r, form)
		return
	}
	if errors.Is(err, store.ErrValidation) {
		s.log.Info("web: schedule add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrForm = scheduleSubmitRefusal
		s.scheduleRefusal(w, r, form)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: schedule add", "schedule", created.Name, "kind", created.Kind, "by", ident.token.Name)
	s.redirectFlash(w, r, "/schedules", "added "+created.Name)
}

// scheduleRefusal re-renders the schedules surface at 422 with the add dialog
// carrying its refusals — the JS-off path, and the fragment body a JS submit
// swaps in place. Refusals never flash: flash is success-only (write.go).
func (s *Server) scheduleRefusal(w http.ResponseWriter, r *http.Request, form scheduleFormVM) {
	vm, err := s.schedulesVMFor(r, &form, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, schedulesList(vm))
}

// schedulesVMFor builds the schedules surface, optionally carrying the add
// dialog's refusal state (nil = fresh form) and a refused row verb's state
// (nil at rest; the refusal renders on the row, forms/SPEC.md).
func (s *Server) schedulesVMFor(r *http.Request, form *scheduleFormVM, refusal *rowRefusal) (schedulesVM, error) {
	rows, err := s.schedules(r)
	if err != nil {
		return schedulesVM{}, err
	}
	if refusal != nil {
		for i := range rows {
			if rows[i].Name == refusal.Name {
				rows[i].Refusal = refusal.Msg
			}
		}
	}
	ident, _ := identityOf(r.Context())
	vm := schedulesVM{
		Page: s.pageFor(r, "Schedules", "schedules"), Schedules: rows,
		Operator: canWrite(ident), Prefill: s.schedulePrefillFor(r),
		Form: form,
	}
	if vm.Operator {
		vm.Page.Add = &pageAdd{Target: "schedules-add", Label: "schedule"}
	}
	return vm, nil
}

func (s *Server) handleScheduleRun(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	sc, err := s.store.GetSchedule(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No schedule named "+name+" exists to run.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}

	specs, err := sched.Specs(r.Context(), s.store, s.log, sc, "operator", s.service().ResolvePlainSourcePlay)
	if err != nil {
		s.log.Error("web: schedule run specs failed", "schedule", name, "err", err.Error(), "by", ident.token.Name)
		s.scheduleRowRefusal(w, r, name, "Could not run schedule: "+err.Error())
		return
	}
	if len(specs) == 0 {
		s.scheduleRowRefusal(w, r, name, "Schedule produced no jobs to run.")
		return
	}

	enq := s.enqueuer()
	var firstJobID int64
	for i, spec := range specs {
		job, err := enq.Enqueue(r.Context(), spec)
		if err != nil {
			s.log.Error("web: schedule run enqueue failed", "schedule", name, "err", err.Error(), "by", ident.token.Name)
			s.scheduleRowRefusal(w, r, name, "Could not enqueue job: "+err.Error())
			return
		}
		if i == 0 {
			firstJobID = job.ID
		}
	}

	s.log.Info("web: schedule run enqueued", "schedule", name, "first_job_id", firstJobID, "count", len(specs), "by", ident.token.Name)
	s.redirectFlash(w, r, "/jobs/"+strconv.FormatInt(firstJobID, 10), "enqueued "+name+" as job #"+strconv.FormatInt(firstJobID, 10))
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
			verb := "enable"
			if !enabled {
				verb = "disable"
			}
			http.Error(w, "No schedule named "+name+" exists to "+verb+".", http.StatusNotFound)
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
	s.redirectFlash(w, r, "/schedules", verb+" "+name)
}

func (s *Server) handleScheduleRm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !r.Form.Has("confirmed") {
		s.scheduleRowRefusal(w, r, name, scheduleRemoveConfirmRefusal)
		return
	}
	if err := s.store.DeleteSchedule(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No schedule named "+name+" exists to remove.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: schedule rm", "schedule", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/schedules", "removed "+name)
}

// scheduleRowRefusal re-renders the schedules surface at 422 with a refused
// row verb's refusal on the refused row — the forms/SPEC.md button-verb shape:
// a refusal renders on the surface that holds the verb, never a flash (flash
// is success-only, write.go).
func (s *Server) scheduleRowRefusal(w http.ResponseWriter, r *http.Request, name, msg string) {
	vm, err := s.schedulesVMFor(r, nil, &rowRefusal{Name: name, Msg: msg})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, schedulesList(vm))
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
