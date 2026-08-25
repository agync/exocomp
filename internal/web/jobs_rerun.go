package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// Job "Run again" resubmits a finished run with a few modifiable things — the
// check (drift) and diff flags, the privileged (sandbox) tier, and the limit /
// tags patterns — without sending the operator back to the source launcher.
// The reconstruction clones the original row's stored JobArgs (everything a run
// needs already lives there, secret material by name only) into a queue.Spec,
// applies the form overrides on the cloned argv, and enqueues a NEW job through
// the same one queue every run write uses. Only names travel — VaultProject /
// RunSecretBinding / Inventory / Source — so a rerun never resurrects a secret
// value into argv (invariant 4).
//
// Privileged is an escalation surface: writeGuard refuses a readonly or approve
// token BEFORE the privileged flag is read, exactly as the source launcher does,
// so a rerun can never privileged-escalate past the operator scope check, and
// the client hiding the toggle is never trusted.
func init() { registerWrite((*Server).registerJobRerunWrite) }

func (s *Server) registerJobRerunWrite(mux *http.ServeMux) {
	mux.HandleFunc("POST /jobs/{id}/rerun", s.requireAuth(s.handleJobRerun))
}

// rerunTrigger labels a job the operator relaunched from a finished one, so the
// job stream and every job face show why this run exists.
const rerunTrigger = "rerun"

func (s *Server) handleJobRerun(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	job, err := s.store.GetJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !job.State.Terminal() {
		http.Error(w, "job "+strconv.FormatInt(id, 10)+" is still "+string(job.State)+
			"; only a finished job can be rerun", http.StatusConflict)
		return
	}
	if len(job.Args.Argv) == 0 {
		http.Error(w, "job "+strconv.FormatInt(id, 10)+" has no command to rerun", http.StatusBadRequest)
		return
	}

	// Clone the original argv, then apply the modal's overrides deterministically:
	// the two drift flags toggle presence, limit/tags replace their value tokens.
	argv := append([]string(nil), job.Args.Argv...)
	argv = setFlag(argv, "--check", r.Form.Has("check"))
	argv = setFlag(argv, "--diff", r.Form.Has("diff"))

	limit := strings.TrimSpace(r.PostFormValue("limit"))
	tags := strings.TrimSpace(r.PostFormValue("tags"))
	if err := validRerunValue("--limit", limit); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validRerunValue("--tags", tags); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	argv = setFlagValue(argv, "--limit", limit)
	argv = setFlagValue(argv, "--tags", tags)

	// Privileged is honored only past writeGuard's operator-scope refusal above;
	// the toggle inherits that gate, never the client's hidden state.
	privileged := r.Form.Has("privileged")

	spec := queue.Spec{
		Runbook: job.Runbook,
		Kind:    job.Kind,
		Trigger: rerunTrigger,
		Argv:    argv,
		// Carried unchanged from the original run: the working directory and its
		// posture, the credential-plane names (never values), the inventory,
		// source, runner, tolerance and timeout.
		Dir:                 job.Args.Dir,
		DirReadOnly:         job.Args.DirReadOnly,
		VaultProject:        job.Args.VaultProject,
		RunSecretBinding:    job.Args.RunSecretBinding,
		Inventory:           job.Args.Inventory,
		Source:              job.Args.Source,
		ExtraVars:           job.Args.ExtraVars,
		RunnerName:          job.Args.RunnerName,
		TolerateUnreachable: job.Args.TolerateUnreachable,
		Timeout:             time.Duration(job.Args.Timeout),
		Privileged:          privileged,
	}

	// The operator who clicked rerun owns the new run, not whoever submitted the
	// original — attribution follows the actor at the keyboard, recorded by the
	// same service tail every run write uses.
	res, err := s.service().Reenqueue(r.Context(), s.enqueuer(), spec,
		service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: rerun", "from", id, "job", res.Job.ID, "privileged", privileged, "by", ident.token.Name)
	redirectFlash(w, r, "/jobs/"+strconv.FormatInt(res.Job.ID, 10),
		"job "+strconv.FormatInt(res.Job.ID, 10)+" enqueued (rerun of "+strconv.FormatInt(id, 10)+")")
}

// setFlag ensures a valueless flag (--check, --diff) is present exactly once when
// want, or wholly absent when not. It strips every existing occurrence first so a
// re-add never duplicates, and position within argv does not matter for these.
func setFlag(argv []string, flag string, want bool) []string {
	out := make([]string, 0, len(argv)+1)
	for _, a := range argv {
		if a == flag {
			continue
		}
		out = append(out, a)
	}
	if want {
		out = append(out, flag)
	}
	return out
}

// setFlagValue sets "flag value" in the two-token form AnsiblePassthrough emits:
// it replaces the token after an existing flag, drops the pair when value is
// empty, or appends the pair when the flag was absent and a value is given.
func setFlagValue(argv []string, flag, value string) []string {
	out := make([]string, 0, len(argv)+2)
	placed := false
	for i := 0; i < len(argv); i++ {
		if argv[i] == flag {
			if i+1 < len(argv) {
				i++ // skip the old value token
			}
			if value != "" && !placed {
				out = append(out, flag, value)
				placed = true
			}
			continue
		}
		out = append(out, argv[i])
	}
	if value != "" && !placed {
		out = append(out, flag, value)
	}
	return out
}

// validRerunValue mirrors the launcher's own flag-value guard: a limit/tags
// override may not carry a newline or NUL. An empty value is fine — it removes
// the flag.
func validRerunValue(flag, value string) error {
	if strings.ContainsAny(value, "\x00\n\r") {
		return errors.New(flag + " value contains a newline or NUL")
	}
	return nil
}
