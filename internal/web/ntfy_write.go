package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// The ntfy leg's write surface (SPEC §2.2): the drawer's Test verb posts one
// push through the daemon's ntfy seam and re-renders with a status line; Save
// persists the Detail two-position switch (notify.ntfy_detail). Both carry
// the operator gate the other browser writes share. The GET /system/ntfy
// drawer render lives here too, so the whole leg surface is one file.

func init() { registerWrite((*Server).registerNtfyWrite) }

func (s *Server) registerNtfyWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /system/ntfy/test", s.requireAuth(s.handleNtfyTest))
	mux.HandleFunc("POST /system/ntfy/save", s.requireAuth(s.handleNtfySave))
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
	detail := ""
	if d, err := s.store.GetSetting(ctx, store.SettingNtfyDetail); err == nil {
		detail = ntfyDetail(string(d))
	}
	return ntfyCardVM{
		State: state, TopicLabel: label, ServerClass: st.ServerClass,
		Detail: detail, URLSealed: st.URLSealed,
		TopicSealed: st.TopicSealed, TokenSealed: st.TokenSealed,
	}
}

// ntfyVMFor builds the drawer page's render state: the leg judgment plus the
// Test verb's status line (empty at rest).
func (s *Server) ntfyVMFor(r *http.Request, status ntfyTestStatus) ntfyVM {
	ident, _ := identityOf(r.Context())
	vm := ntfyVM{
		Page:     s.pageFor(r, "System · ntfy", "system"),
		Operator: canWrite(ident),
		Leg:      s.ntfyCardVM(r.Context()),
		Status:   status,
	}
	vm.Page.Meta = "Push channel"
	vm.Page.BackHref = "/system"
	return vm
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
