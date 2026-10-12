package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"exocomp.dev/exocomp/internal/deadman"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// The ntfy leg's write surface (SPEC §2.2): the drawer's Test verb posts one
// push through the daemon's ntfy seam and re-renders with a status line; Save
// persists the Detail two-position switch (notify.ntfy_detail); a mute's
// clear lifts it (SPEC §7 A8). Both carry the operator gate the other browser
// writes share. The GET /system/ntfy drawer render lives here too, so the
// whole leg surface is one file.

func init() { registerWrite((*Server).registerNtfyWrite) }

func (s *Server) registerNtfyWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /system/ntfy/test", s.requireAuth(s.handleNtfyTest))
	mux.HandleFunc("POST /system/ntfy/save", s.requireAuth(s.handleNtfySave))
	mux.HandleFunc("POST /system/ntfy/configure", s.requireAuth(s.handleNtfyConfigure))
	mux.HandleFunc("POST /system/ntfy/setup", s.requireAuth(s.handleNtfyConfigure))
	mux.HandleFunc("POST /system/ntfy/mutes/{id}/clear", s.requireAuth(s.handleMuteClear))
}

// ntfyLegState derives the leg judgment from the daemon seam, falling back to
// the sealed keys' presence when this surface has none. It re-derives the
// NewNtfy refusal rule (public host + no token) from the seam's judgment —
// presence, never values.
func (s *Server) ntfyLegState(ctx context.Context) (NtfyState, NtfySeamState) {
	st := NtfySeamState{}
	if s.ntfy != nil {
		got, err := s.ntfy.State(ctx)
		if err == nil {
			st = got
		} else {
			s.log.Error("web: ntfy state", "err", err)
		}
	} else if _, err := s.store.GetSecret(ctx, notify.SecretNtfyURL); err == nil {
		st.URLSealed = true
	}
	if !st.URLSealed {
		if u, err := s.store.GetSetting(ctx, store.SettingNtfyURL); err == nil && u != "" {
			st.URLSealed = true
		}
	}
	if !st.TopicSealed {
		if _, err := s.store.GetSecret(ctx, notify.SecretNtfyTopic); err == nil {
			st.TopicSealed = true
		} else if t, err := s.store.GetSetting(ctx, store.SettingNtfyTopic); err == nil && t != "" {
			st.TopicSealed = true
		}
	}
	if !st.TokenSealed {
		if _, err := s.store.GetSecret(ctx, notify.SecretNtfyToken); err == nil {
			st.TokenSealed = true
		}
	}
	if !st.URLSealed {
		return NtfyStateOff, st
	}
	if st.Refused {
		return NtfyStateRefused, st
	}
	return NtfyStateOn, st
}

// ntfyDetail resolves an operator-supplied Detail to one of the two legal
// positions, mirroring notify.normalizeDetail: an unset or unrecognized value
// (a stale build, a typo in the setting table) falls back to the documented
// descriptive default.
func ntfyDetail(raw string) string {
	if raw == string(notify.DetailTerse) {
		return string(notify.DetailTerse)
	}
	return string(notify.DetailDescriptive)
}

// presenceWord is the sealed-key row's SEALED | MISSING word.
func presenceWord(sealed bool) string {
	if sealed {
		return "SEALED"
	}
	return "MISSING"
}

// sealedDots is the dot string a sealed key renders in place of its value — a
// fixed presence glyph, never the length of the secret it stands for (the row
// would otherwise leak the token's size).
func sealedDots(n int) string {
	return strings.Repeat("•", n)
}

// ntfyCardVM builds the leg's render state for the /system card and the
// drawer: the judgment, the topic label, the server class, the Detail setting,
// and the sealed keys' presence.
func (s *Server) ntfyCardVM(ctx context.Context) ntfyCardVM {
	state, st := s.ntfyLegState(ctx)
	label := ""
	if l, err := s.store.GetSetting(ctx, store.SettingNtfyTopic); err == nil {
		label = l
	}
	cfgURL := ""
	if u, err := s.store.GetSetting(ctx, store.SettingNtfyURL); err == nil {
		cfgURL = u
	}
	detail := ""
	if d, err := s.store.GetSetting(ctx, store.SettingNtfyDetail); err == nil {
		detail = ntfyDetail(string(d))
	}
	return ntfyCardVM{
		State: state, TopicLabel: label, ConfiguredURL: cfgURL, ServerClass: st.ServerClass,
		Detail: detail, URLSealed: st.URLSealed,
		TopicSealed: st.TopicSealed, TokenSealed: st.TokenSealed,
	}
}

// ntfyVMFor builds the drawer page's render state: the leg judgment, the
// MUTES list, and the Test verb's status line (empty at rest).
func (s *Server) ntfyVMFor(r *http.Request, status ntfyTestStatus) ntfyVM {
	ident, _ := identityOf(r.Context())
	vm := ntfyVM{
		Page:     s.pageFor(r, "System · ntfy", "system"),
		Operator: canWrite(ident),
		Leg:      s.ntfyCardVM(r.Context()),
		Status:   status,
		Mutes:    s.muteRows(r.Context()),
	}
	vm.Page.Meta = "Push channel"
	vm.Page.BackHref = "/system"
	return vm
}

// muteRows projects the drawer's MUTES section (SPEC §7 A8): the active
// notification mutes, newest first, through the SAME store.ListNotifyMutes
// the API's mute list and the banner read. Source is the alert name for an
// alert-scoped mute, "global" for a global one; a host-scoped mute fills the
// host cell. Until is the countdown face ("in 2h 05m"). A global mute is the
// safety valve: clearing it asks first.
func (s *Server) muteRows(ctx context.Context) []muteRow {
	mutes, err := s.store.ListNotifyMutes(ctx, true)
	if err != nil {
		s.log.Error("web: mutes", "err", err)
		return nil
	}
	now := s.now()
	var out []muteRow
	for _, m := range mutes {
		row := muteRow{
			ID:     m.ID,
			Source: "—",
			Host:   "—",
			Until:  "in " + deadman.CountdownAge(m.ExpiresAt.Sub(now)),
		}
		switch m.ScopeKind {
		case store.ScopeAlert:
			row.Source = m.ScopeValue
		case store.ScopeHost:
			row.Host = m.ScopeValue
		default:
			row.Source = "global"
			row.ClearConfirm = "Clear the global mute? Every notification pages again."
		}
		out = append(out, row)
	}
	return out
}

// ntfyDriverOrUnavailable returns the daemon's ntfy seam, or renders a 503 and
// reports false when this surface has none (a surface built without one cannot
// push).
func (s *Server) ntfyDriverOrUnavailable(w http.ResponseWriter) (NtfyDriver, bool) {
	if s.ntfy == nil {
		http.Error(w, "this surface holds no ntfy channel; run the push test from the daemon's web surface",
			http.StatusServiceUnavailable)
		return nil, false
	}
	return s.ntfy, true
}

// handleNtfy renders the /system/ntfy drawer (SPEC §2.2): the sealed-key
// presence rows, the Detail switch, WHAT A PUSH SAYS, the fixed fan-out table,
// and the Test/Save verbs.
func (s *Server) handleNtfy(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, viewNtfy(s.ntfyVMFor(r, ntfyTestStatus{})))
}

// handleNtfyTest posts one test push through the daemon seam and re-renders
// the drawer with the status line: ok copy on delivery, the refusal copy when
// the channel cannot load (a public topic without a token), or a delivery
// failure sentence. The push never carries a secret — it names host and clock
// (copy.txt).
func (s *Server) handleNtfyTest(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	drv, ok := s.ntfyDriverOrUnavailable(w)
	if !ok {
		return
	}
	host := hostname()
	if host == "" {
		host = "exocomp"
	}
	err := drv.TestPush(r.Context(), host)
	if err != nil {
		if errors.Is(err, ErrNtfyRefused) {
			s.render(w, r, http.StatusOK, viewNtfy(s.ntfyVMFor(r,
				ntfyTestStatus{Class: "fail", Msg: "the server refused the token"})))
		} else {
			s.log.Error("web: ntfy test push", "err", err)
			s.render(w, r, http.StatusOK, viewNtfy(s.ntfyVMFor(r,
				ntfyTestStatus{Class: "fail", Msg: "the push was not delivered"})))
		}
		return
	}
	now := s.now().Format("15:04:05")
	s.render(w, r, http.StatusOK, viewNtfy(s.ntfyVMFor(r,
		ntfyTestStatus{Class: "ok", Msg: "test delivered · " + now + " · " + s.ntfyCardVM(r.Context()).TopicLabel})))
	s.log.Info("web: ntfy test push sent", "by", ident.token.Name)
}

// handleNtfySave persists the Detail two-position switch. The value is
// normalized to the two legal positions, so a stale build or a hand-typed form
// cannot persist anything the transport would misread.
func (s *Server) handleNtfySave(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	raw := strings.TrimSpace(r.PostFormValue("detail"))
	detail := ntfyDetail(raw)
	if err := s.store.PutSetting(r.Context(), store.SettingNtfyDetail, detail); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: ntfy detail saved", "detail", detail, "by", ident.token.Name)
	s.redirectFlash(w, r, "/system/ntfy", "push detail saved")
}

// handleMuteClear lifts one mute before it expires, through the SAME
// store.DeleteNotifyMute the CLI's `mute rm` and the API's DELETE /mutes
// call. Clearing is a direct store call — a mute is a row, not a job, so no
// queue hop adds guardrails the store does not already enforce. A global mute
// asked first in the drawer; an unknown id fails closed, never a silent
// success.
func (s *Server) handleMuteClear(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		s.fail(w, r, fmt.Errorf("web: clear mute: bad id"))
		return
	}
	if !r.Form.Has("confirmed") {
		s.redirectFlash(w, r, "/system/ntfy#mutes", "clear requires confirmation")
		return
	}
	if err := s.store.DeleteNotifyMute(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: mute cleared", "id", strconv.FormatInt(id, 10), "by", ident.token.Name)
	s.redirectFlash(w, r, "/system/ntfy#mutes", "mute cleared")
}

// handleNtfyConfigure saves the contextual modal's configuration: Server URL
// and Topic are non-secrets saved into settings (and sealed in secrets if sealer
// is available). If an access token/secret is provided, it is auto-created in
// sealed secrets as ntfy/token.
func (s *Server) handleNtfyConfigure(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	rawURL := strings.TrimSpace(r.PostFormValue("url"))
	topic := strings.TrimSpace(r.PostFormValue("topic"))
	token := strings.TrimSpace(r.PostFormValue("token"))
	from := r.PostFormValue("from")
	if from != "/system/ntfy" {
		from = "/system"
	}

	form := ntfyModalVM{
		URL:   rawURL,
		Topic: topic,
	}

	u, err := url.Parse(rawURL)
	if rawURL == "" {
		form.ErrURL = "Give the ntfy server URL."
	} else if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		form.ErrURL = "Enter a valid HTTP or HTTPS URL — like https://ntfy.sh"
	}

	if topic == "" {
		form.ErrTopic = "Give the topic name."
	}

	if form.ErrURL == "" && u != nil {
		if notify.IsPublicNtfyHost(u.Hostname()) {
			_, tokenErr := s.store.GetSecret(r.Context(), notify.SecretNtfyToken)
			hasToken := tokenErr == nil
			if token == "" && !hasToken {
				form.ErrToken = "ntfy.sh requires an access token to protect approval links — self-host ntfy, or provide an access token"
			}
		}
	}

	if form.HasError() {
		s.log.Info("web: ntfy configure refused", "errURL", form.ErrURL, "errTopic", form.ErrTopic, "errToken", form.ErrToken, "by", ident.token.Name)
		s.ntfyModalRefusal(w, r, from, &form)
		return
	}

	if token != "" && s.sealer == nil {
		form.ErrForm = "Sealing is not available on this surface."
		s.ntfyModalRefusal(w, r, from, &form)
		return
	}

	if err := s.store.PutSetting(r.Context(), store.SettingNtfyURL, rawURL); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.PutSetting(r.Context(), store.SettingNtfyTopic, topic); err != nil {
		s.fail(w, r, err)
		return
	}

	if s.sealer != nil {
		if err := s.sealSecret(r.Context(), notify.SecretNtfyURL, "url", rawURL); err != nil {
			s.log.Error("web: seal ntfy url", "err", err)
		}
		if err := s.sealSecret(r.Context(), notify.SecretNtfyTopic, "topic", topic); err != nil {
			s.log.Error("web: seal ntfy topic", "err", err)
		}
		if token != "" {
			if err := s.sealSecret(r.Context(), notify.SecretNtfyToken, "token", token); err != nil {
				s.log.Error("web: seal ntfy token", "err", err)
			}
		}
	}

	s.log.Info("web: ntfy configured", "by", ident.token.Name, "url", rawURL, "topic", topic)
	s.redirectFlash(w, r, from, "ntfy configured")
}

func (s *Server) ntfyModalRefusal(w http.ResponseWriter, r *http.Request, from string, form *ntfyModalVM) {
	if from == "/system/ntfy" {
		vm := s.ntfyVMFor(r, ntfyTestStatus{})
		vm.Modal = form
		s.render(w, r, http.StatusUnprocessableEntity, viewNtfy(vm))
		return
	}
	vm, err := s.systemVMFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.NtfyModal = form
	s.render(w, r, http.StatusUnprocessableEntity, systemPage(vm))
}
