package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// A check names a probe set — targets and an interval — never a secret. So,
// like schedules, this lane is operator-scope only: no reauth, no
// confirm_token. The name=spec targets are parsed by the shared
// store.ParseCheckTargets (blank-skipping for the form's empty rows), so a
// check added from the browser is held to exactly the same per-kind rules as
// one added from the CLI. Refusals re-render the dialog at 422 with every
// field's message at once (forms/SPEC.md); the raw validator words go to the
// log, never the operator.

func init() { registerWrite((*Server).registerCheckWrite) }

func (s *Server) registerCheckWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /checks/add", s.requireAuth(s.handleCheckAdd))
	mux.HandleFunc("POST /checks/{name}/update", s.requireAuth(s.handleCheckUpdate))
	mux.HandleFunc("POST /checks/{name}/enable", s.requireAuth(s.handleCheckEnable))
	mux.HandleFunc("POST /checks/{name}/disable", s.requireAuth(s.handleCheckDisable))
	mux.HandleFunc("POST /checks/{name}/rm", s.requireAuth(s.handleCheckRm))
}

// The add dialog's human refusal copy (forms/copy.txt): one sentence in the
// field's terms, never the validator's regex or a status code.
const (
	checkNameCharset       = "Names use letters, digits, dots, hyphens and underscores."
	checkIntervalRequired  = "Give the interval as a duration — like 5m or 90s."
	checkIntervalBad       = "The interval needs a unit — like 5m, 90s or 1h."
	checkIntervalNonPos    = "The interval must be more than zero — like 5m."
	checkKindRefusal       = "Kind is tcp, ping or http."
	checkTargetEmpty       = "Add at least one target, like laptop=192.168.1.50:22."
	checkExpectRefusal     = "The expected status is a whole number, like 200."
	checkExpectKindRefusal = "An expected status pins an http target only."
	checkSubmitRefusal     = "Couldn't add the check — nothing was saved."
	// checkRemoveConfirmRefusal is the rm verb's confirm-gate refusal
	// (forms/SPEC.md): the shared confirm dialog's hidden confirmed=1 is the
	// safety, and a confirmed-less remove is refused on the row.
	checkRemoveConfirmRefusal = "Nothing was removed — confirm the removal in the dialog."
)

func checkNameRefusal(name string) (msg, fix string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Give the check a name.", ""
	}
	if strings.Contains(name, " ") {
		msg = "Names can't have spaces."
	} else {
		msg = checkNameCharset
	}
	return msg, netgroupSlug(name)
}

func checkIntervalRefusal(v string) string {
	if strings.TrimSpace(v) == "" {
		return checkIntervalRequired
	}
	return checkIntervalBad
}

func checkDuplicateRefusal(name string) string {
	return "A check named " + name + " already exists."
}

// checkTargetRefusal translates a ParseCheckTargets refusal into the target
// group's sentence (forms/copy.txt), naming the offending spec mono and
// unquoted. The parser's raw words stay in the error, which the caller logs.
func checkTargetRefusal(err error) (item, msg string) {
	raw := err.Error()
	item = raw
	if rest, ok := strings.CutPrefix(raw, `"`); ok {
		if end := strings.IndexByte(rest, '"'); end >= 0 {
			item = rest[:end]
		}
	}
	switch {
	case strings.Contains(raw, "must be name=spec"):
		return item, " must be name=spec — like laptop=192.168.1.50:22."
	case strings.Contains(raw, "must be ip:port"):
		return item, " is a tcp target needing ip:port — like laptop=192.168.1.50:22."
	case strings.Contains(raw, "is not a number"):
		return item, " port isn't a number — like laptop=192.168.1.50:22."
	default:
		return item, " isn't a target this kind can probe."
	}
}

func (s *Server) handleCheckAdd(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	form := checkFormVM{
		Name: r.PostFormValue("name"), Kind: r.PostFormValue("kind"),
		Interval: r.PostFormValue("interval"), Targets: r.PostForm["target"],
		Expect: r.PostFormValue("expect"),
	}

	// Validate every field up front so all refusals render at once (forms/
	// SPEC.md); the daemon stays authoritative on uniqueness and the ping
	// capability probe.
	var causes []string
	if !store.ValidName(form.Name) {
		form.ErrName, form.FixName = checkNameRefusal(form.Name)
		causes = append(causes, "name "+form.Name)
	}
	d, err := time.ParseDuration(form.Interval)
	if err != nil {
		form.ErrInterval = checkIntervalRefusal(form.Interval)
		causes = append(causes, err.Error())
	} else if d <= 0 {
		form.ErrInterval = checkIntervalNonPos
		causes = append(causes, "interval "+form.Interval)
	}
	validKind := form.Kind == store.CheckTCP || form.Kind == store.CheckPing || form.Kind == store.CheckHTTP
	if !validKind {
		form.ErrKind = checkKindRefusal
		causes = append(causes, "kind "+form.Kind)
	}
	var targets []store.CheckTarget
	if validKind {
		parsed, terr := store.ParseCheckTargets(form.Kind, form.Targets, true)
		if terr != nil {
			form.ErrTargetItem, form.ErrTarget = checkTargetRefusal(terr)
			causes = append(causes, terr.Error())
		} else if len(parsed) == 0 {
			form.ErrTarget = checkTargetEmpty
			causes = append(causes, "no targets")
		} else {
			targets = parsed
		}
	}
	expect, eerr := formExpect(r)
	if eerr != nil {
		form.ErrExpect = checkExpectRefusal
		causes = append(causes, eerr.Error())
	} else if expect != 0 && form.Kind != store.CheckHTTP {
		form.ErrExpect = checkExpectKindRefusal
		causes = append(causes, "expect on non-http")
	}

	if form.ErrName != "" || form.ErrKind != "" || form.ErrInterval != "" ||
		form.ErrTarget != "" || form.ErrExpect != "" {
		switch {
		case form.ErrName != "":
			form.FirstInvalid = "name"
		case form.ErrKind != "":
			form.FirstInvalid = "kind"
		case form.ErrInterval != "":
			form.FirstInvalid = "interval"
		case form.ErrTarget != "":
			form.FirstInvalid = "target"
		case form.ErrExpect != "":
			form.FirstInvalid = "expect"
		}
		s.log.Info("web: check add refused", "err", strings.Join(causes, "; "), "by", ident.token.Name)
		s.checkRefusal(w, r, form)
		return
	}
	if expect != 0 {
		for i := range targets {
			targets[i].Expect = expect
		}
	}

	created, err := s.store.CreateCheck(r.Context(), store.NewCheck{
		Name: form.Name, Kind: form.Kind, IntervalS: int64(d.Seconds()), Targets: targets,
	})
	if errors.Is(err, store.ErrDuplicate) {
		s.log.Info("web: check add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrName = checkDuplicateRefusal(form.Name)
		form.FirstInvalid = "name"
		s.checkRefusal(w, r, form)
		return
	}
	if errors.Is(err, store.ErrValidation) {
		s.log.Info("web: check add refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrForm = checkSubmitRefusal
		s.checkRefusal(w, r, form)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: check add", "check", created.Name, "kind", created.Kind, "by", ident.token.Name)
	s.redirectFlash(w, r, "/checks", "added "+created.Name)
}

// checkRefusal re-renders the checks surface at 422 with the add dialog
// carrying its refusals — the JS-off path, and the fragment body a JS submit
// swaps in place. Refusals never flash: flash is success-only (write.go).
func (s *Server) checkRefusal(w http.ResponseWriter, r *http.Request, form checkFormVM) {
	vm, err := s.checksVMFor(r, nil, &form, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, checksList(vm))
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
			verb := "enable"
			if !enabled {
				verb = "disable"
			}
			http.Error(w, "No check named "+name+" exists to "+verb+".", http.StatusNotFound)
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
	name := r.PathValue("name")
	if !r.Form.Has("confirmed") {
		s.checkRowRefusal(w, r, name, checkRemoveConfirmRefusal)
		return
	}
	if err := s.store.DeleteCheck(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "No check named "+name+" exists to remove.", http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: check rm", "check", name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/checks", "removed "+name)
}

// checkRowRefusal re-renders the checks surface at 422 with a refused row
// verb's refusal on the refused check card — the forms/SPEC.md button-verb
// shape: a refusal renders on the surface that holds the verb, never a flash
// (flash is success-only, write.go).
func (s *Server) checkRowRefusal(w http.ResponseWriter, r *http.Request, name, msg string) {
	vm, err := s.checksVMFor(r, nil, nil, &rowRefusal{Name: name, Msg: msg})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusUnprocessableEntity, checksList(vm))
}

// checkUpdateSubmitRefusal is the edit form's form-level sentence when the
// store refuses a value the field checks let through.
const checkUpdateSubmitRefusal = "Couldn't save the check — nothing changed."

// checkEditFormVM is one card's edit dialog state: the interval and target rows
// prefilled from the stored check at rest, or the typed values plus every
// field's refusal on a 422 re-render. Kind is display-only — the stored kind is
// never updatable. Control ids are namespaced by the check name, so each card's
// dialog has unique controls.
type checkEditFormVM struct {
	Name          string
	Kind          string
	Interval      string
	Targets       []string
	Expect        string
	ErrInterval   string
	ErrTargetItem string
	ErrTarget     string
	ErrExpect     string
	ErrForm       string
	FirstInvalid  string
	Submitting    bool
}

func (f checkEditFormVM) effective() checkEditFormVM { return f }

func (f checkEditFormVM) Pending() bool { return f.Submitting }

func (f checkEditFormVM) ErrFormText() string { return f.ErrForm }

func (f checkEditFormVM) intervalField() fieldMeta {
	return fieldMeta{
		ID: "cke-" + f.Name + "-interval", Label: "Interval (a duration)",
		Name: "interval", Value: f.Interval, Placeholder: "5m",
		Help:   "A Go duration — 5m, 90s, 1h.",
		ErrMsg: f.ErrInterval, FirstInvalid: f.FirstInvalid == "interval",
	}
}

func (f checkEditFormVM) expectField() fieldMeta {
	return fieldMeta{
		ID: "cke-" + f.Name + "-expect", Label: "HTTP expected status (optional, http kind only)",
		Name: "expect", Value: f.Expect, Placeholder: "200",
		Help:   "A whole status code, like 200 or 404.",
		ErrMsg: f.ErrExpect, FirstInvalid: f.FirstInvalid == "expect",
	}
}

// targetInputs is the edit dialog's repeated target rows: the stored targets
// (or the typed values on a 422 re-render) padded to the same four baseline
// rows the add dialog uses, capped at the same ten.
func (f checkEditFormVM) targetInputs() []string {
	out := f.Targets
	for i := len(out); i < 4; i++ {
		out = append(out, "")
	}
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

// editForm resolves one card's edit-dialog state: the refused form when this
// check was the one refused, else an at-rest form prefilled from the stored
// row.
func (vm checksVM) editForm(name string) checkEditFormVM {
	if vm.EditForm != nil && vm.EditForm.Name == name {
		return *vm.EditForm
	}
	for _, ck := range vm.Checks {
		if ck.Name == name {
			return checkEditFormVM{
				Name: name, Kind: ck.Kind, Interval: ck.Interval,
				Targets: ck.editTargetInputs(), Expect: ck.editExpect(),
			}
		}
	}
	return checkEditFormVM{Name: name}
}

func (c checkRow) editTargetInputs() []string {
	out := make([]string, len(c.Targets))
	for i, t := range c.Targets {
		out[i] = t.SpecInput()
	}
	return out
}

func (c checkRow) editExpect() string {
	for _, t := range c.Targets {
		if t.Expect != 0 {
			return itoa(int64(t.Expect))
		}
	}
	return ""
}

// handleCheckUpdate re-points a stored check in place: targets replace the set
// and interval replaces the cadence, while kind stays whatever the row held.
// The store preserves the state row of a kept target NAME, so the next sweep
// can flip it and emit a real transition. Refusals re-render the dialog at 422
// with every field's message at once (forms/SPEC.md).
func (s *Server) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	cur, err := s.store.GetCheck(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "No check named "+name+" exists to update.", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	form := checkEditFormVM{
		Name:     name,
		Kind:     cur.Kind,
		Interval: r.PostFormValue("interval"),
		Targets:  r.PostForm["target"],
		Expect:   r.PostFormValue("expect"),
	}

	var causes []string
	var intervalS int64
	d, derr := time.ParseDuration(form.Interval)
	if derr != nil {
		form.ErrInterval = checkIntervalRefusal(form.Interval)
		causes = append(causes, derr.Error())
	} else if d <= 0 {
		form.ErrInterval = checkIntervalNonPos
		causes = append(causes, "interval "+form.Interval)
	} else {
		intervalS = int64(d.Seconds())
	}
	var targets []store.CheckTarget
	parsed, terr := store.ParseCheckTargets(cur.Kind, form.Targets, true)
	if terr != nil {
		form.ErrTargetItem, form.ErrTarget = checkTargetRefusal(terr)
		causes = append(causes, terr.Error())
	} else if len(parsed) == 0 {
		form.ErrTarget = checkTargetEmpty
		causes = append(causes, "no targets")
	} else {
		targets = parsed
	}
	expect, eerr := formExpect(r)
	if eerr != nil {
		form.ErrExpect = checkExpectRefusal
		causes = append(causes, eerr.Error())
	} else if expect != 0 && cur.Kind != store.CheckHTTP {
		form.ErrExpect = checkExpectKindRefusal
		causes = append(causes, "expect on non-http")
	}

	if form.ErrInterval != "" || form.ErrTarget != "" || form.ErrExpect != "" {
		switch {
		case form.ErrInterval != "":
			form.FirstInvalid = "interval"
		case form.ErrTarget != "":
			form.FirstInvalid = "target"
		case form.ErrExpect != "":
			form.FirstInvalid = "expect"
		}
		s.log.Info("web: check update refused", "check", name, "err", strings.Join(causes, "; "), "by", ident.token.Name)
		s.checkEditRefusal(w, r, form)
		return
	}
	if expect != 0 {
		for i := range targets {
			targets[i].Expect = expect
		}
	}

	updated, err := s.store.UpdateCheck(r.Context(), name, targets, intervalS)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "No check named "+name+" exists to update.", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrValidation) {
		s.log.Info("web: check update refused", "check", name, "err", err.Error(), "by", ident.token.Name)
		form.ErrForm = checkUpdateSubmitRefusal
		s.checkEditRefusal(w, r, form)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: check update", "check", updated.Name, "by", ident.token.Name)
	s.redirectFlash(w, r, "/checks", "updated "+updated.Name)
}

// checkEditRefusal re-renders the checks surface at 422 with the edit dialog
// carrying its refusals; the dialog's own open script reopens it (the JS-off
// path, and the full-page re-render a native submit lands on).
func (s *Server) checkEditRefusal(w http.ResponseWriter, r *http.Request, form checkEditFormVM) {
	vm, err := s.checksVMFor(r, nil, nil, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.EditForm = &form
	s.render(w, r, http.StatusUnprocessableEntity, checksList(vm))
}
