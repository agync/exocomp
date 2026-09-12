package web

import (
	"errors"
	"net/http"
	"strings"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/deadman"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// This file mounts the dead-man's write surface: Arm (threshold + sealed
// heartbeat URL), Disarm (both halves removed, asks first on the card), and
// the TRIPPED card's Queue backup verb. Arm and Disarm write only settings and
// the sealed secret the daemon's trip monitor reads; the monitor itself never
// lives here (it runs in the daemon, internal/deadman). The heartbeat value is
// a sealed secret from the moment it leaves this handler — it never re-renders
// (invariant 4).
func init() { registerWrite((*Server).registerDeadmanWrite) }

func (s *Server) registerDeadmanWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /system/deadman/arm", s.requireAuth(s.handleDeadmanArm))
	mux.HandleFunc("POST /system/deadman/disarm", s.requireAuth(s.handleDeadmanDisarm))
	mux.HandleFunc("POST /system/deadman/queue", s.requireAuth(s.handleDeadmanQueue))
}

// deadmanThresholds are the Arm dialog's segmented values (SPEC §1.2): exactly
// one is on, and the server accepts nothing else — a crafted value can neither
// invent a threshold the dialog never offered nor smuggle a duration past the
// freshness ladder.
var deadmanThresholds = []string{"24h", "48h", "72h", "7d"}

func deadmanThresholdValid(v string) bool {
	for _, k := range deadmanThresholds {
		if v == k {
			return true
		}
	}
	return false
}

// deadmanThresholdRefusal and the heartbeat/meta sentences are the dialog's
// human copy (forms/SPEC.md): the raw validator word goes to the log, the meta
// line carries these sentences.
const (
	deadmanThresholdRefusal = "Pick one of the offered thresholds — 24h, 48h, 72h or 7d."
	deadmanHeartbeatRefusal = "The heartbeat URL must be an https URL with no embedded credentials that resolves to public addresses."
	deadmanSubmitRefusal    = "The dead-man could not be armed — the daemon could not seal the heartbeat URL."
	deadmanDisarmRefusal    = "The dead-man could not be disarmed."
)

// fromPage normalizes the hidden from field to one of the two pages that carry
// the card: /system (the default) or /system/deadman.
func fromPage(from string) string {
	if from == "/system/deadman" {
		return from
	}
	return "/system"
}

// handleDeadmanArm writes both halves of the dead-man: the threshold setting
// and, when a URL was pasted, the sealed deadman/heartbeat_url. The threshold
// alone arms the internal half ("heartbeat: not wired" — SPEC §1.2), so the
// URL field is optional; a pasted URL is validated and sealed, never echoed.
func (s *Server) handleDeadmanArm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	from := fromPage(r.PostFormValue("from"))
	form := deadmanFormVM{Threshold: strings.TrimSpace(r.PostFormValue("threshold"))}

	if !deadmanThresholdValid(form.Threshold) {
		form.ErrThreshold = deadmanThresholdRefusal
		form.FirstInvalid = "threshold"
	}
	heartbeat := strings.TrimSpace(r.PostFormValue("heartbeat_url"))
	if heartbeat != "" {
		if _, err := notify.ValidatePushEndpoint(r.Context(), heartbeat); err != nil {
			s.log.Info("web: deadman arm refused", "err", err.Error(), "by", ident.token.Name)
			form.ErrHeartbeat = deadmanHeartbeatRefusal
			if form.FirstInvalid == "" {
				form.FirstInvalid = "heartbeat"
			}
		}
	}
	if form.ErrThreshold != "" || form.ErrHeartbeat != "" {
		s.deadmanRefusal(w, r, from, &form)
		return
	}

	if s.sealer == nil {
		http.Error(w, "sealing is not available on this surface", http.StatusServiceUnavailable)
		return
	}
	if heartbeat != "" {
		sealed, err := s.sealer.Seal(deadman.SecretHeartbeatURL, "url", []byte(heartbeat))
		if err != nil {
			s.log.Error("web: deadman arm", "err", err.Error())
			form.ErrForm = deadmanSubmitRefusal
			s.deadmanRefusal(w, r, from, &form)
			return
		}
		if _, gerr := s.store.GetSecret(r.Context(), deadman.SecretHeartbeatURL); gerr == nil {
			// Re-arming replaces the sealed value in place; the envelope row's
			// lifecycle timestamps stay the secret's own.
			if err := s.store.RotateSecret(r.Context(), deadman.SecretHeartbeatURL,
				sealed.Nonce, sealed.Ciphertext); err != nil {
				s.fail(w, r, err)
				return
			}
		} else {
			if _, err := s.store.CreateSecret(r.Context(), store.SecretRow{
				Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce,
				Ciphertext: sealed.Ciphertext, AAD: sealed.Name + "|" + sealed.Kind,
			}); err != nil {
				if !errors.Is(err, store.ErrDuplicate) {
					s.fail(w, r, err)
					return
				}
			}
		}
	}
	if err := s.store.PutSetting(r.Context(), store.SettingDeadmanThreshold, form.Threshold); err != nil {
		s.fail(w, r, err)
		return
	}
	// A fresh arm starts the markers clean: a re-armed dead-man pages again only
	// if the new threshold trips, and the new heartbeat URL's ping clock starts
	// empty rather than inheriting the old one's.
	_ = s.store.DeleteSetting(r.Context(), store.SettingDeadmanPagedAt)
	_ = s.store.DeleteSetting(r.Context(), store.SettingDeadmanPingedAt)
	s.log.Info("web: deadman arm", "threshold", form.Threshold,
		"heartbeat", heartbeat != "", "by", ident.token.Name)
	s.redirectFlash(w, r, from, "dead-man armed")
}

// handleDeadmanDisarm removes both halves: the threshold setting and the sealed
// heartbeat URL, plus the trip markers — the card returns to NONE and the
// monitor stops watching.
func (s *Server) handleDeadmanDisarm(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	from := fromPage(r.PostFormValue("from"))
	if err := s.store.DeleteSetting(r.Context(), store.SettingDeadmanThreshold); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.DeleteSecret(r.Context(), deadman.SecretHeartbeatURL); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, err)
			return
		}
	}
	_ = s.store.DeleteSetting(r.Context(), store.SettingDeadmanPagedAt)
	_ = s.store.DeleteSetting(r.Context(), store.SettingDeadmanPingedAt)
	s.log.Info("web: deadman disarm", "by", ident.token.Name)
	s.redirectFlash(w, r, from, "dead-man disarmed")
}

// handleDeadmanQueue is the TRIPPED card's primary verb: queue one backup for
// the newest-enabled target through the same queue every run write uses (the
// web never drains — the daemon claims and runs the row). The verb's job is
// kind backup, exactly the shape exocomp backup run enqueues.
func (s *Server) handleDeadmanQueue(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	from := fromPage(r.PostFormValue("from"))
	name := strings.TrimSpace(r.PostFormValue("target"))
	target, err := s.store.GetBackupTarget(r.Context(), name)
	if err != nil || !target.Enabled {
		http.Error(w, "no such enabled backup target: "+name, http.StatusBadRequest)
		return
	}
	res, err := s.service().Reenqueue(r.Context(), s.enqueuer(), queue.Spec{
		Runbook: "backup", Kind: backup.JobKind, Trigger: "manual",
		Argv: backup.Argv(target.Name), ConcurrencyKey: backup.ConcurrencyKey(target.Name),
	}, service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name, Face: "web"})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: deadman queue backup", "target", target.Name, "job", res.Job.ID, "by", ident.token.Name)
	s.redirectFlash(w, r, from, "backup queued for "+target.Name)
}
