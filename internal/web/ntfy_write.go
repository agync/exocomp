package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// The ntfy leg's write surface (SPEC §2). There is no drawer: the whole channel
// — server URL, topic, access token, push detail and the test — lives in the one
// ntfy modal on /system. Save persists the configuration and returns to /system;
// Send test saves the same form, posts one push through the daemon seam, and
// re-renders the modal open with the status box so a failure is debuggable. Both
// carry the operator gate the other browser writes share.

func init() { registerWrite((*Server).registerNtfyWrite) }

func (s *Server) registerNtfyWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /system/ntfy/configure", s.requireAuth(s.handleNtfyConfigure))
	mux.HandleFunc("POST /system/ntfy/setup", s.requireAuth(s.handleNtfyConfigure))
	mux.HandleFunc("POST /system/ntfy/test", s.requireAuth(s.handleNtfyTest))
}

// ntfyLegState derives the leg judgment from the daemon seam, falling back to
// the URL's presence when this surface has none. It re-derives the NewNtfy
// refusal rule (public host + no token) from the seam's judgment — presence,
// never values.
func (s *Server) ntfyLegState(ctx context.Context) (NtfyState, NtfySeamState) {
	st := NtfySeamState{}
	if s.ntfy != nil {
		got, err := s.ntfy.State(ctx)
		if err == nil {
			st = got
		} else {
			s.log.Error("web: ntfy state", "err", err)
		}
	}
	if !st.URLSealed {
		// The URL is an ordinary setting now, so a setting wins and a legacy
		// sealed ntfy/url is the fallback (the same precedence the transport's
		// lookup uses).
		if u, err := s.store.GetSetting(ctx, store.SettingNtfyURL); err == nil && u != "" {
			st.URLSealed = true
		} else if _, err := s.store.GetSecret(ctx, notify.SecretNtfyURL); err == nil {
			st.URLSealed = true
		}
	}
	if !st.TopicSealed {
		if t, err := s.store.GetSetting(ctx, store.SettingNtfyTopic); err == nil && t != "" {
			st.TopicSealed = true
		} else if _, err := s.store.GetSecret(ctx, notify.SecretNtfyTopic); err == nil {
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

// ntfyCardVM builds the leg's render state for the /system card and the modal:
// the judgment, the topic label, the configured URL, the server class, the
// Detail setting, and the keys' presence (the token is the one that is sealed).
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

// ntfyApply parses and validates the modal form and, when it passes, persists
// it: Server URL and Topic are ordinary (unsealed) settings, the push detail is
// notify.ntfy_detail, and only the optional access token is sealed as
// ntfy/token. It then clears any legacy sealed ntfy/url or ntfy/topic left by an
// install from before the settings move — the readers are secret-first, so a
// stale row would silently win over the setting just saved. It returns the form
// VM carrying any refusal, and ok=false when it refused.
func (s *Server) ntfyApply(ctx context.Context, r *http.Request) (*ntfyModalVM, bool) {
	rawURL := strings.TrimSpace(r.PostFormValue("url"))
	topic := strings.TrimSpace(r.PostFormValue("topic"))
	token := strings.TrimSpace(r.PostFormValue("token"))
	detail := ntfyDetail(strings.TrimSpace(r.PostFormValue("detail")))

	form := &ntfyModalVM{URL: rawURL, Topic: topic}

	u, err := url.Parse(rawURL)
	switch {
	case rawURL == "":
		form.ErrURL = "Give the ntfy server URL."
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		form.ErrURL = "Enter a valid HTTP or HTTPS URL — like https://ntfy.sh"
	case u.User != nil:
		form.ErrURL = "Put credentials in the access token, not in the URL."
	}

	if topic == "" {
		form.ErrTopic = "Give the topic name."
	}

	if form.ErrURL == "" && u != nil && notify.IsPublicNtfyHost(u.Hostname()) {
		_, tokenErr := s.store.GetSecret(ctx, notify.SecretNtfyToken)
		if token == "" && tokenErr != nil {
			form.ErrToken = "ntfy.sh requires an access token to protect approval links — self-host ntfy, or provide an access token"
		}
	}

	if form.HasError() {
		return form, false
	}
	if token != "" && s.sealer == nil {
		form.ErrForm = "Sealing is not available on this surface."
		return form, false
	}

	if err := s.store.PutSetting(ctx, store.SettingNtfyURL, rawURL); err != nil {
		s.log.Error("web: ntfy url setting", "err", err)
		form.ErrForm = "Could not save the configuration."
		return form, false
	}
	if err := s.store.PutSetting(ctx, store.SettingNtfyTopic, topic); err != nil {
		s.log.Error("web: ntfy topic setting", "err", err)
		form.ErrForm = "Could not save the configuration."
		return form, false
	}
	if err := s.store.PutSetting(ctx, store.SettingNtfyDetail, detail); err != nil {
		s.log.Error("web: ntfy detail setting", "err", err)
		form.ErrForm = "Could not save the configuration."
		return form, false
	}
	if token != "" {
		if err := s.sealSecret(ctx, notify.SecretNtfyToken, "token", token); err != nil {
			s.log.Error("web: seal ntfy token", "err", err)
			form.ErrForm = "Could not seal the access token."
			return form, false
		}
	}
	s.clearLegacyNtfySecrets(ctx)
	return form, true
}

// clearLegacyNtfySecrets removes a sealed ntfy/url or ntfy/topic an install may
// still carry from before those became settings. Absent is the normal case.
func (s *Server) clearLegacyNtfySecrets(ctx context.Context) {
	for _, name := range []string{notify.SecretNtfyURL, notify.SecretNtfyTopic} {
		if err := s.store.DeleteSecret(ctx, name); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Error("web: clear legacy ntfy secret", "name", name, "err", err)
		}
	}
}

// handleNtfyConfigure is the modal's Save: it persists the configuration and
// returns to /system with a flash. A refusal re-renders the modal open (422)
// with the typed values kept.
func (s *Server) handleNtfyConfigure(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	form, ok := s.ntfyApply(r.Context(), r)
	if !ok {
		s.log.Info("web: ntfy configure refused", "errURL", form.ErrURL, "errTopic", form.ErrTopic, "errToken", form.ErrToken, "by", ident.token.Name)
		s.renderNtfyModal(w, r, form, ntfyTestStatus{})
		return
	}
	// The URL and topic are settings, never logged here; only the host, which is
	// not a secret, names the target.
	s.log.Info("web: ntfy configured", "host", ntfyHost(form.URL), "by", ident.token.Name)
	s.redirectFlash(w, r, "/system", "ntfy configuration saved")
}

// handleNtfyTest is the modal's Send test: it saves the same form, posts one
// push through the daemon seam, and re-renders the modal open with the status
// box — the ok line, the refusal copy, or a delivery-failure sentence. The push
// never carries a secret: it names host and clock only.
func (s *Server) handleNtfyTest(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	drv, ok := s.ntfyDriverOrUnavailable(w)
	if !ok {
		return
	}
	form, ok := s.ntfyApply(r.Context(), r)
	if !ok {
		s.renderNtfyModal(w, r, form, ntfyTestStatus{})
		return
	}
	host := hostname()
	if host == "" {
		host = "exocomp"
	}
	if err := drv.TestPush(r.Context(), host); err != nil {
		if errors.Is(err, ErrNtfyRefused) {
			s.renderNtfyModal(w, r, nil, ntfyTestStatus{Class: "fail", Msg: "the server refused the token"})
		} else {
			s.log.Error("web: ntfy test push", "err", err)
			s.renderNtfyModal(w, r, nil, ntfyTestStatus{Class: "fail", Msg: ntfyFailReason(err)})
		}
		return
	}
	now := s.now().Format("15:04:05")
	s.renderNtfyModal(w, r, nil, ntfyTestStatus{Class: "ok",
		Msg: "test delivered · " + now + " · " + s.ntfyCardVM(r.Context()).TopicLabel})
	s.log.Info("web: ntfy test push sent", "by", ident.token.Name)
}

// ntfyHost is the URL's host for a log line — never the whole URL, which can
// carry credentials in its userinfo.
func ntfyHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Hostname()
	}
	return "unparseable"
}

// renderNtfyModal re-renders /system with the ntfy modal OPEN, carrying either a
// refusal form (422) or a test status (200).
func (s *Server) renderNtfyModal(w http.ResponseWriter, r *http.Request, form *ntfyModalVM, status ntfyTestStatus) {
	vm, err := s.systemVMFor(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	vm.NtfyModal = form
	vm.NtfyTest = status
	code := http.StatusOK
	if form != nil && form.HasError() {
		code = http.StatusUnprocessableEntity
	}
	s.render(w, r, code, systemPage(vm))
}

// ntfyFailReason turns a delivery error into one plain sentence the operator
// can act on. It never echoes the error itself — a dial error can carry the
// server host, and a secret never reaches the page — so it classifies a few
// common transport failures and falls back to the generic sentence.
func ntfyFailReason(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "the push timed out — the server did not answer"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host"):
		return "the server host was not found"
	case strings.Contains(msg, "connection refused"):
		return "the server refused the connection"
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"):
		return "the server's TLS certificate was not trusted"
	}
	return "the push was not delivered"
}
