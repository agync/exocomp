package web

import (
	"context"
	"net/http"
	"strings"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/litestream"
)

// The litestream leg's write surface (SPEC §7 A6): the Set-target form on
// /system/litestream persists the replica target and the unencrypted-remote
// ack — the two settings, nothing else. The daemon cannot verify a replica
// target (no reachability probe exists), so the form never pretends to: it
// writes the settings and echoes the CLI. A remote target without the ack is
// refused at persistence, the same caveat the CLI enforces (cmd/exocomp/
// litestream.go target). The page's GET render lives here too, so the whole
// Set-target surface is one file.

func init() { registerWrite((*Server).registerLitestreamWrite) }

func (s *Server) registerLitestreamWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /system/litestream/target", s.requireAuth(s.handleLitestreamSetTarget))
}

// The Set-target form's human copy (forms/SPEC.md): the raw validator word goes
// to the log, the meta lines carry these sentences.
const (
	litestreamTargetRefusal = "The replica target must parse as a path, file://, s3:// or sftp:// URI."
	litestreamAckRefusal    = "A remote target is not encrypted by Litestream — tick the acknowledgement to proceed."
	litestreamSubmitRefusal = "The replica target could not be saved."
	litestreamPageMeta      = "the fast-RPO leg · set the target, then the sidecar replicates"
)

// litestreamPageVMFor builds the /system/litestream surface: the full-width
// card, the ▾ config view, and the Set-target form holding the stored target
// and ack.
func (s *Server) litestreamPageVMFor(r *http.Request) litestreamPageVM {
	vm := litestreamPageVM{
		Page: s.pageFor(r, "System · Litestream", "system"),
		LS:   s.litestreamVM(r.Context()),
		Form: s.litestreamFormAtRest(r.Context()),
	}
	vm.Page.Meta = litestreamPageMeta
	return vm
}

// litestreamFormAtRest reads the two settings for the form's at-rest controls:
// the stored target verbatim and the stored ack.
func (s *Server) litestreamFormAtRest(ctx context.Context) litestreamFormVM {
	f := litestreamFormVM{}
	if raw, err := s.store.GetSetting(ctx, litestream.SettingReplicaTarget); err == nil {
		f.Target = raw
	}
	if v, err := s.store.GetSetting(ctx, litestream.SettingReplicaAck); err == nil {
		f.Ack = v == "true"
	}
	return f
}

// handleLitestreamSetTarget persists the replica target and the ack — the two
// settings, nothing else — because the daemon cannot verify a target. A remote
// target without the ack is refused on the checkbox (the same caveat the CLI
// enforces at persistence); a local target writes ack=false, since the caveat
// only applies to remote targets.
func (s *Server) handleLitestreamSetTarget(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	form := litestreamFormVM{Target: strings.TrimSpace(r.PostFormValue("replica_target"))}
	form.Ack = r.PostFormValue("replica_unencrypted_ack") != ""

	target, perr := litestream.ParseTarget(form.Target)
	if perr != nil {
		form.ErrTarget = litestreamTargetRefusal
		form.FirstInvalid = "replica_target"
	} else if target.Remote() && !form.Ack {
		form.ErrAck = litestreamAckRefusal
		form.FirstInvalid = "replica_unencrypted_ack"
	}
	if form.ErrTarget != "" || form.ErrAck != "" {
		s.litestreamRefusal(w, r, &form)
		return
	}

	if err := s.store.PutSetting(r.Context(), litestream.SettingReplicaTarget, form.Target); err != nil {
		s.fail(w, r, err)
		return
	}
	ackValue := "false"
	if form.Ack {
		ackValue = "true"
	}
	if err := s.store.PutSetting(r.Context(), litestream.SettingReplicaAck, ackValue); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: litestream target set", "remote", target.Remote(),
		"cleartext-creds", backup.HasURIUserinfo(form.Target), "by", ident.token.Name)
	s.redirectFlash(w, r, "/system/litestream", "replica target set — the sidecar does the rest")
}

// litestreamRefusal re-renders the /system/litestream page at 422 holding the
// refused form, so the field refusals and the typed target survive JS-off.
func (s *Server) litestreamRefusal(w http.ResponseWriter, r *http.Request, form *litestreamFormVM) {
	vm := litestreamPageVM{
		Page: s.pageFor(r, "System · Litestream", "system"),
		LS:   s.litestreamVM(r.Context()),
		Form: *form,
	}
	vm.Page.Meta = litestreamPageMeta
	s.render(w, r, http.StatusUnprocessableEntity, litestreamPage(vm))
}
