package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// killReason is the fixed audit reason a web-triggered global kill records.
const killReason = "web: global kill"

// handleKill drives the approval surface's "Silence & global kill" control
// (view_approval.templ, form action "/kill"): the hold-to-confirm button that
// pauses the reactive engine everywhere, not just the one alert being decided.
//
// It creates a global-scope silence — the SAME mechanism `exocomp silence add
// --global` already uses (cmd/exocomp/silence.go) and the ONLY thing
// dashboard_data.go's banner() reads to decide the PAUSED state — so a
// successful kill renders PAUSED on the very next render. It deliberately does
// NOT go through the Maintenance/cordon seam (api_maintenance.go): cordon
// pauses the daemon's OWN scheduler and closes its job-claim gate, a
// per-process in-memory toggle meant for a planned reboot, and doing so would
// stop schedules firing — which contradicts this control's own copy ("Running
// jobs finish. Schedules still fire."). A global silence is also the only one
// of the two a standalone `exocomp web` process (no daemon, s.maint == nil)
// can still drive: it is a plain store row the daemon's reactive engine reads,
// the same way a CLI-created silence already crosses process boundaries.
//
// The write floor matches the rest of the write surface: only the operator
// scope may enqueue a mutating action (canWrite, api.go), so a readonly or
// approve-only token is refused before anything is touched.
func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	ident, _ := identityOf(r.Context())
	if !canWrite(ident) {
		http.Error(w, "this token may not trigger the global kill switch", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	expiry := r.PostFormValue("expiry")
	form := jobKillForm{Expiry: expiry}
	approvalID, _ := strconv.Atoi(r.PostFormValue("approval_id"))
	approvalID64 := int64(approvalID)
	// The hold-to-confirm gesture is client-side UX (globalKill's onpointerdown
	// timer); the server re-asserts it so a stray or scripted POST without the
	// hold cannot fire the switch. A missed hold is the status line's refusal;
	// a bad expiry the field's — both re-render this screen at 422 (forms
	// pattern), never a flash.
	if r.PostFormValue("confirmed") != "1" {
		s.log.Info("web: kill refused", "err", "not held", "by", ident.token.Name)
		form.ErrForm = killHoldRefusal
		s.killRefusal(w, r, approvalID64, form)
		return
	}
	dur, err := time.ParseDuration(expiry)
	if err != nil || dur <= 0 {
		s.log.Info("web: kill refused", "err", "bad expiry", "by", ident.token.Name)
		form.ErrExpiry = killExpiryRefusal
		form.FirstInvalid = "expiry"
		s.killRefusal(w, r, approvalID64, form)
		return
	}

	now := s.now()
	if _, err := s.store.CreateSilence(r.Context(), store.Silence{
		ScopeKind:  store.ScopeGlobal,
		Reason:     killReason,
		CreatedAt:  now,
		ExpiresAt:  now.Add(dur),
		CreatedVia: "web",
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, s.url("/"), http.StatusSeeOther)
}

// The kill form's human refusal copy (the forms pattern): the raw validator
// words go to the log; the field and status lines carry these sentences.
const (
	killExpiryRefusal = "The expiry needs a positive duration — like 2h or 45m."
	killHoldRefusal   = "Hold the button to confirm the pause."
)

// killRefusal re-renders the approval surface the kill form was on at 422 with
// the form's refusals filled. The approval id travels as a hidden field so the
// re-render lands on the same decision screen.
func (s *Server) killRefusal(w http.ResponseWriter, r *http.Request, approvalID int64, form jobKillForm) {
	if approvalID <= 0 {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	vm, err := s.approvalVM(r, approvalID, "")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		s.fail(w, r, err)
		return
	}
	vm.Kill = &form
	s.render(w, r, http.StatusUnprocessableEntity, approvalSurface(vm))
}
