package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
)

// The source detail page is the run surface: every browser run is now launched
// in the context of a registered source. GET /sources/{name} renders a header
// (kind, trust, ref, last sync) plus a Playbook | Command launcher; the two
// POST /sources/{name}/run/* routes are the browser form face of the same
// enqueue verbs the CLI and the /api/v1 write API call (RunPlaybook, RunAdhoc),
// so a browser operator and an API/CLI caller reach the queue through the SAME
// path (AGENTS.md invariant 5). Like the old /run surface, a run is not
// reauth-gated: it mints no credential and seals nothing, so the operator scope
// check in writeGuard is the whole gate. --require-approval on the command form
// routes the job through the existing approval gate instead of running it.

func init() { registerWrite((*Server).registerSourceDetail) }

func (s *Server) registerSourceDetail(mux *http.ServeMux) {
	mux.HandleFunc("GET /sources/{name}", s.requireAuth(s.handleSourceDetail))
	mux.HandleFunc("POST /sources/{name}/run/playbook", s.requireAuth(s.handleSourceRunPlaybook))
	mux.HandleFunc("POST /sources/{name}/run/command", s.requireAuth(s.handleSourceRunCommand))
}

// routeCommand namespaces an ad-hoc command run's idempotency key, alongside
// the api_write.go route labels (routePlan/routeApply/routePlaybook).
const routeCommand = "command.run"

// playbookOther is the sentinel the launcher's playbook <select> carries for its
// "other path…" option: it means "ignore the select, use the free-path input".
const playbookOther = "__other__"

// sourceDetailVM is the source detail page: a read-only header for everyone and,
// for an operator, a Playbook | Command launcher prefilled from the query string
// so the page is deep-linkable (?tab=playbook&playbook=site.yml).
type sourceDetailVM struct {
	Page     page
	Operator bool
	Source   sourceRow
	Writable bool
	// Playbooks are the checkout's discovered playbook paths (relative,
	// slash-separated) the Playbook tab offers as a <select>; a free-path input
	// covers anything discovery missed.
	Playbooks []string
	// Tab is the launcher tab shown first ("playbook" or "command"), from the
	// ?tab= query param.
	Tab string
	// Playbook prefills the Playbook tab from ?playbook=; if it is one of the
	// discovered paths it is pre-selected, otherwise it seeds the free-path input.
	Playbook string
	// Launch opens the launch drawer already open — the JS-off fallback for the
	// ?launch=1 query, which renders the <dialog> with its open attribute.
	Launch bool
}

// PlaybookKnown reports whether the prefilled playbook is one of the discovered
// paths, so the template can decide between pre-selecting it in the <select> and
// seeding the free-path input.
func (vm sourceDetailVM) PlaybookKnown() bool {
	for _, p := range vm.Playbooks {
		if p == vm.Playbook {
			return true
		}
	}
	return false
}

// FreePrefill is the playbook value to seed the free-path input with: the
// prefill only when it is not a discovered path (an unknown path is a free-path
// run), empty otherwise.
func (vm sourceDetailVM) FreePrefill() string {
	if vm.Playbook != "" && !vm.PlaybookKnown() {
		return vm.Playbook
	}
	return ""
}

// RunLabel names the thing the launch drawer's submit button queues: the
// prefilled known playbook, or the path placeholder when discovery found none.
// The button states the contract ("Queue run — <thing> @ <ref>", F4) so the
// operator sees exactly what the one queue will run.
func (vm sourceDetailVM) RunLabel() string {
	if vm.PlaybookKnown() {
		return vm.Playbook
	}
	if len(vm.Playbooks) == 0 {
		return "a path"
	}
	return "a playbook"
}

// QueueLabel is the launch drawer's submit-button contract: the thing being
// queued and the ref it resolves against, so the operator sees exactly what the
// one queue will run (F4). The `@ ref` is omitted when the source has no ref.
func (vm sourceDetailVM) QueueLabel() string {
	if vm.Source.Ref == "" {
		return "Queue run — " + vm.RunLabel()
	}
	return "Queue run — " + vm.RunLabel() + " @ " + vm.Source.Ref
}

// PlaybooksNote is the PLAYBOOKS card's line when discovery found nothing: it
// names the reason — a never-synced source (no checkout to read) vs a synced
// source with an unusual layout.
func (vm sourceDetailVM) PlaybooksNote() string {
	if vm.Source.NeverSynced() {
		return "None discovered — this source has never synced. Sync brings the checkout down and reads its layout."
	}
	return "None discovered in the checkout — give a path relative to it in the launcher."
}

// LaunchMeta is the drawer's provenance line — which source (and ref, when it
// has one) the run resolves against, and that it queues rather than executes
// directly. `@ {ref}` is omitted for a ref-less source.
func (vm sourceDetailVM) LaunchMeta() string {
	if vm.Source.Ref == "" {
		return vm.Source.Name + " · queues through the one queue"
	}
	return vm.Source.Name + " @ " + vm.Source.Ref + " · queues through the one queue"
}

func (s *Server) handleSourceDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := service.ValidSourceName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	src, err := s.store.SourceByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no source named "+name, http.StatusNotFound)
			return
		}
		s.fail(w, r, err)
		return
	}

	ident, _ := identityOf(r.Context())
	vm := sourceDetailVM{
		Page:     s.pageFor(r, src.Name, "sources"),
		Operator: canWrite(ident),
		Source: sourceRow{
			Name: src.Name, Kind: src.Kind, URI: src.URI, Ref: src.Ref,
			SyncSHA: src.LastSyncSHA, Signed: src.Signed,
			Signers: src.AllowedSigners != "", Enabled: src.Enabled,
		},
		Writable: src.Writable,
		Tab:      "playbook",
		Playbook: r.URL.Query().Get("playbook"),
		Launch:   r.URL.Query().Get("launch") == "1",
	}
	if r.URL.Query().Get("tab") == "command" {
		vm.Tab = "command"
	}

	// Discovery walks the checkout; a not-yet-synced source (root absent) yields
	// an empty list rather than an error, so the launcher still renders.
	if loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI, src.Writable); err == nil {
		if pbs, perr := service.ListSourcePlaybooks(loc.Root); perr == nil {
			vm.Playbooks = pbs
		}
	}

	s.render(w, r, http.StatusOK, sourceDetail(vm))
}

// parseFormEgress turns the launch drawer's egress field into the validated
// port set a run carries. The field may name several ports comma-separated;
// junk — a non-integer or a port outside 1..65535 — is refused here, at the
// form parse, so a typo never reaches a job row.
func parseFormEgress(r *http.Request) ([]int, error) {
	var ports []int
	for _, v := range r.Form["egress"] {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("egress port %q is not a number", part)
			}
			ports = append(ports, n)
		}
	}
	return store.NormalizeEgressPorts(ports)
}

// handleSourceRunPlaybook mirrors apiPlaybookRun (api_write.go): the same
// service.ResolveSourcePlaybook/ResolveInventory resolution and the same
// service.RunPlaybook call, differing only in the Enqueuer (this listener's
// pure-enqueue one) and in rendering a redirect instead of a JSON envelope. The
// source is the path segment now, not a form field, so every browser run reads
// from a registered source's own tree.
func (s *Server) handleSourceRunPlaybook(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := service.ValidSourceName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The launcher's <select> carries a discovered path, or the "other path…"
	// sentinel that defers to the free-path input; either way the resolved value
	// is confined to the source checkout below.
	playbook := r.PostFormValue("playbook")
	if playbook == "" || playbook == playbookOther {
		playbook = r.PostFormValue("playbook_path")
	}
	if playbook == "" {
		http.Error(w, "playbook is required", http.StatusBadRequest)
		return
	}
	timeout, err := parseOptionalDuration(r, "timeout")
	if err != nil {
		http.Error(w, "timeout: "+err.Error(), http.StatusBadRequest)
		return
	}

	passthrough, vars, err := service.AnsiblePassthrough(
		r.PostFormValue("limit"), r.PostFormValue("tags"), "",
		r.Form.Has("check"), r.Form.Has("diff"), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	egressPorts, err := parseFormEgress(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	svc := s.service()
	override := filepath.Join(s.dataDir, "hooks", name+".yml")
	hooked, abs, serr := svc.ResolveSourcePlaybook(r.Context(), override, name, playbook)
	if errors.Is(serr, service.ErrSourcePathEscape) {
		http.Error(w, serr.Error(), http.StatusBadRequest)
		return
	}
	if errors.Is(serr, store.ErrNotFound) {
		http.Error(w, "no source named "+name, http.StatusNotFound)
		return
	}
	if serr != nil {
		s.fail(w, r, serr)
		return
	}
	if hooked {
		http.Error(w, "source "+name+" declares hooks; its lifecycle workflow is not offered on this surface yet",
			http.StatusNotImplemented)
		return
	}
	playbook = abs

	invPath, invName, err := svc.ResolveInventory(r.Context(), r.PostFormValue("inventory"))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// Run from the source checkout so its ./ansible.cfg (and the relative
	// inventory/roles it declares) is honored — the same working directory the
	// CLI and the ad-hoc handler grant. Without it the run falls back to the job
	// scratch dir and the checkout's config is never discovered.
	root, readOnly, err := s.resolveSourceCheckout(r.Context(), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// The privileged (unsandboxed) tier is reachable only through this
	// operator-scoped write handler (writeGuard already refused a readonly or
	// approve token above), so the checkbox inherits that gate — the same
	// session/token scope every other run control on this surface checks.
	privileged := r.Form.Has("privileged")
	res, err := svc.RunPlaybook(r.Context(), s.enqueuer(), service.PlaybookInput{
		PlaybookPath:        playbook,
		InventoryPath:       invPath,
		InventoryName:       invName,
		RunSecrets:          r.PostFormValue("run_secrets"),
		Source:              name,
		Dir:                 root,
		DirReadOnly:         readOnly,
		Passthrough:         passthrough,
		ExtraVars:           vars,
		EgressPorts:         egressPorts,
		Privileged:          privileged,
		TolerateUnreachable: r.Form.Has("tolerate_unreachable"),
		IdempotencyKey:      idempotencyKey(r, routePlaybook),
		Timeout:             timeout,
		Actor:               service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name, Face: "web"},
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("web: run playbook", "job", res.Job.ID, "source", name, "playbook", playbook,
		"privileged", privileged, "by", ident.token.Name)
	redirectFlash(w, r, "/jobs/"+strconv.FormatInt(res.Job.ID, 10), "job "+strconv.FormatInt(res.Job.ID, 10)+" enqueued")
}

// handleSourceRunCommand parses the everyday ad-hoc form (host, command, shell,
// become, inventory, run_secrets, require_approval, timeout) and hands the
// resolved input to service.RunAdhoc, which assembles the queue.Spec and either
// enqueues it plainly or routes it through the approval gate. The command runs
// with the source checkout (from the path segment) as its working directory.
func (s *Server) handleSourceRunCommand(w http.ResponseWriter, r *http.Request) {
	ident, ok := s.writeGuard(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := service.ValidSourceName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	host := r.PostFormValue("host")
	command := r.PostFormValue("command")
	if command == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	module := "command"
	if r.Form.Has("shell") {
		module = "shell"
	}
	timeout, err := parseOptionalDuration(r, "timeout")
	if err != nil {
		http.Error(w, "timeout: "+err.Error(), http.StatusBadRequest)
		return
	}

	dir, dirReadOnly, err := s.resolveSourceCheckout(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no source named "+name, http.StatusNotFound)
			return
		}
		if errors.Is(err, service.ErrSourcePathEscape) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.fail(w, r, err)
		return
	}

	svc := s.service()
	invPath, invName, err := svc.ResolveInventory(r.Context(), r.PostFormValue("inventory"))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	res, err := svc.RunAdhoc(r.Context(), s.enqueuer(), s.deriver, service.AdhocInput{
		Runbook:         "command",
		Pattern:         host,
		Module:          module,
		Args:            command,
		Become:          r.Form.Has("become"),
		Dir:             dir,
		DirReadOnly:     dirReadOnly,
		InventoryPath:   invPath,
		InventoryName:   invName,
		RunSecrets:      r.PostFormValue("run_secrets"),
		Source:          name,
		RequireApproval: r.Form.Has("require_approval"),
		// Operator-only, gated by writeGuard above just like every other control
		// on this surface; the checkbox only reaches here past that scope check.
		Privileged:     r.Form.Has("privileged"),
		IdempotencyKey: idempotencyKey(r, routeCommand),
		Timeout:        timeout,
		Actor:          service.Actor{Kind: service.ActorKindToken, Name: ident.token.Name, Face: "web"},
	})
	if err != nil {
		if errors.Is(err, service.ErrSourcePathEscape) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A guard refusal (bad host pattern/module/args) is a bad request, not a
		// server fault; RunAdhoc returns those as plain errors with no store
		// sentinel to match on, so a validation-shaped message is rendered 400.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if res.Gated {
		a := res.Approval
		s.log.Info("web: run command gated", "job", a.JobID, "approval", a.ApprovalID, "source", name, "by", ident.token.Name)
		redirectFlash(w, r, "/approvals",
			"job "+strconv.FormatInt(a.JobID, 10)+" gated; approval "+strconv.FormatInt(a.ApprovalID, 10)+" pending")
		return
	}
	s.log.Info("web: run command", "job", res.Run.Job.ID, "source", name, "host", host, "by", ident.token.Name)
	redirectFlash(w, r, "/jobs/"+strconv.FormatInt(res.Run.Job.ID, 10),
		"job "+strconv.FormatInt(res.Run.Job.ID, 10)+" enqueued")
}

// resolveSourceCheckout resolves a registered source's checkout root and whether
// a run may write to it, the web mirror of cmd/exocomp/adhoc.go's
// sourceCheckoutRoot.
func (s *Server) resolveSourceCheckout(ctx context.Context, name string) (root string, readOnly bool, err error) {
	if err := service.ValidSourceName(name); err != nil {
		return "", false, err
	}
	src, err := s.store.SourceByName(ctx, name)
	if err != nil {
		return "", false, err
	}
	loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return "", false, err
	}
	return loc.Root, loc.ReadOnly, nil
}

// parseOptionalDuration parses a form field as a Go duration; an empty value
// means no limit.
func parseOptionalDuration(r *http.Request, field string) (time.Duration, error) {
	v := r.PostFormValue(field)
	if v == "" {
		return 0, nil
	}
	return time.ParseDuration(v)
}
