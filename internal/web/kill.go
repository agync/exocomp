package web

import (
	"net/http"
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
	// The hold-to-confirm gesture is client-side UX (globalKill's onpointerdown
	// timer); the server re-asserts it so a stray or scripted POST without the
	// hold cannot fire the switch.
	if r.PostFormValue("confirmed") != "1" {
		http.Error(w, "hold to confirm is required", http.StatusBadRequest)
		return
	}
	dur, err := time.ParseDuration(r.PostFormValue("expiry"))
	if err != nil || dur <= 0 {
		http.Error(w, "expiry must be a positive duration (e.g. 2h)", http.StatusBadRequest)
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
