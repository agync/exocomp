package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
	// discovered paths it is pre-selected, otherwise it seeds the free-path
	// input. With no prefill, the first discovered playbook is preselected
	// (SPEC §5.2 — the launcher states one concrete run before any input).
	Playbook string
	// Launch opens the launch drawer already open — the JS-off fallback for the
	// ?launch=1 query, which renders the <dialog> with its open attribute.
	Launch bool
	// Egress carries the source's current run posture — the last playbook run's
	// declared ports (SPEC §5.2, "prefilled from the source default"). Empty is
	// the port-22 default. The Egress field's value and the CLI echo format it
	// differently (display "22, 443" vs --egress 22,443).
	Egress []int
	// LastRunID/LastRunState name the source's newest playbook job for the
	// launcher meta ("last run job #N · state"); LastRunID 0 = nothing has run.
	LastRunID    int64
	LastRunState string
	// RunForm is the launch drawer's refusal state (the forms pattern): nil at
	// rest, the filled form on a 422 re-render with the drawer open on the
	// refused tab. It echoes the drawer's own typed values back — never a
	// secret, and there is no confirm_token on this surface — so the operator's
	// input survives a refusal.
	RunForm *sourceRunForm
	// RmRefusal is the delete verb's confirm-gate refusal: the shared confirm
	// dialog's hidden confirmed=1 is the safety, and a confirmed-less remove
	// re-renders the detail page at 422 with this on the form-status line.
	// Empty at rest.
	RmRefusal string
	// SyncRefusal is the sync verb's refusal copy shown on the detail page
	// when a sync fails or cannot run. Empty at rest.
	SyncRefusal string
	// DefaultEgress is the operator-set default egress set every ansible run
	// naming this source inherits, on top of its declared ports (Decision
	// 0023). Empty renders no Egress ports line — the quiet default posture.
	DefaultEgress []int
	// EgressInput is the egress-default form's typed value on a 422
	// re-render, so a refused set keeps what the operator entered; empty falls
	// back to the current default.
	EgressInput string
	// EgressRefusal is the egress-default verb's refusal copy shown on the
	// detail page when a set is refused (a junk or out-of-range port). Empty
	// at rest.
	EgressRefusal string
}

// sourceRunForm is one launcher tab's refusal state: the typed values an
// operator submitted, the toggle states, and the field/status refusals the
// handler attached. Empty Err* means the field is at rest.
type sourceRunForm struct {
	Tab      string // "playbook" | "command"
	Playbook string // the select's value (discovered path, __other__, or "")
	FreePath string
	Limit    string
	Egress   string
	Timeout  string
	Host     string
	Command  string
	// The drawer's toggle states, kept across a refusal so a re-render loses
	// nothing the operator chose.
	Check, Diff, Tolerate, Privileged bool // playbook tab
	Shell, Become, RequireApproval    bool // command tab
	ErrPlaybook                       string
	ErrLimit                          string
	ErrEgress                         string
	ErrTimeout                        string
	ErrHost                           string
	ErrCommand                        string
	ErrForm                           string
	FirstInvalid                      string
	Submitting                        bool
}

func (f *sourceRunForm) effective() sourceRunForm {
	if f == nil {
		return sourceRunForm{}
	}
	return *f
}

func (f *sourceRunForm) Pending() bool {
	return f != nil && f.Submitting
}

func (f *sourceRunForm) ErrFormText() string {
	if f == nil {
		return ""
	}
	return f.ErrForm
}

// limitField and egressField build the Playbook tab's typed fields (the forms
// pattern): the field's help at rest, the refusal sentence on a 422 re-render.
// Egress falls back to the source's current posture when the form carried no
// typed value.
func (f *sourceRunForm) limitField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "rp-limit", Label: "Limit", Name: "limit", Value: v.Limit,
		Placeholder: "all hosts", Help: "ansible --limit pattern",
		ErrMsg: errOf(f, "limit"), FirstInvalid: firstInvalid(f, "limit"),
	}
}

func (f *sourceRunForm) egressField(def string) fieldMeta {
	v := f.effective()
	val := v.Egress
	if val == "" {
		val = def
	}
	return fieldMeta{
		ID: "rp-egress", Label: "Egress ports (TCP, comma-separated)", Name: "egress",
		Value: val, Help: "Ports the runner may reach — same as --egress.",
		ErrMsg: errOf(f, "egress"), FirstInvalid: firstInvalid(f, "egress"),
	}
}

// timeoutField is the shared Timeout control (both tabs carry one); id is the
// field's own (rp-timeout / rc-timeout).
func (f *sourceRunForm) timeoutField(id string) fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: id, Label: "Timeout (optional, e.g. 10m)", Name: "timeout",
		Value: v.Timeout, Placeholder: "no limit",
		Help:   "A duration — like 10m or 1h; empty means no limit.",
		ErrMsg: errOf(f, "timeout"), FirstInvalid: firstInvalid(f, "timeout"),
	}
}

func (f *sourceRunForm) hostField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "rc-host", Label: "Host pattern", Name: "host", Value: v.Host,
		Placeholder: "web or fedora:!nas",
		Help:        "Ansible host pattern — web, fedora:!nas, or all.",
		ErrMsg:      errOf(f, "host"), FirstInvalid: firstInvalid(f, "host"),
	}
}

func (f *sourceRunForm) commandField() fieldMeta {
	v := f.effective()
	return fieldMeta{
		ID: "rc-command", Label: "Command", Name: "command", Value: v.Command,
		Placeholder: "systemctl status nginx",
		Help:        "The command the matched hosts run.",
		ErrMsg:      errOf(f, "command"), FirstInvalid: firstInvalid(f, "command"),
	}
}

// errOf is the refused field's sentence for a nil form (empty = at rest);
// firstInvalid reports whether the field is the one to focus.
func errOf(f *sourceRunForm, field string) string {
	if f == nil {
		return ""
	}
	switch field {
	case "limit":
		return f.ErrLimit
	case "egress":
		return f.ErrEgress
	case "timeout":
		return f.ErrTimeout
	case "host":
		return f.ErrHost
	case "command":
		return f.ErrCommand
	}
	return ""
}

func firstInvalid(f *sourceRunForm, field string) bool {
	return f != nil && f.FirstInvalid == field
}

// The launcher's human refusal copy (the forms pattern): the raw validator
// words go to the log; the drawer's meta and status lines carry these
// sentences.
const (
	sourcePlaybookRefusal  = "Choose a playbook, or give a path relative to the checkout."
	sourceLimitRefusal     = "The limit is one host pattern — it can't carry a newline."
	sourceEgressRefusal    = "Egress ports are numbers from 1 to 65535 — like 22, 443."
	sourceTimeoutRefusal   = "The timeout is a duration — like 10m or 1h."
	sourceHostRefusal      = "Give a host pattern — like web or fedora:!nas."
	sourceCommandRefusal   = "Give the command to run."
	sourceRunSubmitRefusal = "That run can't be enqueued — fix the host pattern or command."
	// sourceRemoveConfirmRefusal is the rm verb's confirm-gate refusal: the
	// shared confirm dialog's hidden confirmed=1 is the safety, and a
	// confirmed-less remove is refused on the detail page.
	sourceRemoveConfirmRefusal = "Nothing was removed — confirm the removal in the dialog."
)

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

// RunLabel names the thing a launch queues: the prefilled known playbook, or
// the path placeholder when discovery found none. The drawer footer states the
// contract with it ("same as exocomp run <source> <thing>", F4) so the operator
// sees exactly what the one queue will run; the submit button itself is the
// bare verb, so a long collection path can never wrap it.
func (vm sourceDetailVM) RunLabel() string {
	if vm.PlaybookKnown() {
		return vm.Playbook
	}
	if len(vm.Playbooks) == 0 {
		return "a path"
	}
	return "a playbook"
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

// EgressPrefill is the Egress field's value (SPEC §5.2): the source's current
// run posture's ports, comma-and-space joined — the way the designer's field
// reads ("22, 443"). Empty is the port-22 default posture.
func (vm sourceDetailVM) EgressPrefill() string { return joinPorts(vm.Egress, ", ") }

// EgressDefaultLabel is the identity card's Egress ports value: the operator-set
// default egress set joined for display ("443, 2222"). The template renders the
// row only when it is non-empty, so an undeclared default adds no line.
func (vm sourceDetailVM) EgressDefaultLabel() string { return joinPorts(vm.DefaultEgress, ", ") }

// EgressDefaultField is the egress-default form's field state: the typed value
// kept on a 422 re-render, else the current default; the refusal sentence on a
// refusal, else the at-rest help. It never carries a secret.
func (vm sourceDetailVM) EgressDefaultField() fieldMeta {
	v := vm.EgressInput
	if v == "" {
		v = joinPorts(vm.DefaultEgress, ", ")
	}
	return fieldMeta{
		ID: "src-egress", Label: "Egress ports (TCP, comma-separated)", Name: "egress",
		Value: v, Placeholder: "443, 2222",
		Help:   "The default every run from this source inherits. Empty clears it.",
		ErrMsg: vm.EgressRefusal,
	}
}

// EgressCliEcho is the egress card's one-line contract (the page echoes CLI
// commands): the exact host command that records the same default. Ports join
// bare (the CLI's space-separated argument form), and an undeclared default
// echoes the placeholder a set would replace.
func (vm sourceDetailVM) EgressCliEcho() string {
	line := "same as exocomp source egress " + vm.Source.Name
	if ports := joinPorts(vm.DefaultEgress, " "); ports != "" {
		return line + " " + ports
	}
	return line + " <ports...>"
}

// PlaybooksMeta is the Playbook select's always-present meta (SPEC §5.2): "N
// playbooks found at {ref}" plus the source's last playbook run when one
// exists. A ref-less source omits "at {ref}".
func (vm sourceDetailVM) PlaybooksMeta() string {
	line := itoa(int64(len(vm.Playbooks))) + " playbooks found"
	if vm.Source.Ref != "" {
		line += " at " + vm.Source.Ref
	}
	if vm.LastRunID != 0 {
		line += " · last run job #" + itoa64(vm.LastRunID) + " · " + vm.LastRunState
	}
	return line
}

// PlaybookHref is the launcher prefill link a discovered playbook row carries
// (P13): ?launch=1 opens the drawer, ?playbook= preselects this path on the
// Playbook tab. The path is query-escaped so a nested "deploy/site.yml" round-
// trips through the ?playbook= parsing.
func (vm sourceDetailVM) PlaybookHref(pb string) string {
	return urlWithBase(vm.Page.Base, "/sources/"+vm.Source.Name+"?launch=1&tab=playbook&playbook="+url.QueryEscape(pb))
}

// CliEcho is the launcher's one-line contract below the primary (SPEC §5.2):
// the exact CLI equivalent of what Queue run submits, so the operator sees the
// run's whole ask at once. The --egress flag appears only when ports are
// declared, joined bare (the CLI's comma form).
func (vm sourceDetailVM) CliEcho() string {
	line := "asks nothing else · same as exocomp run " + vm.Source.Name + " " + vm.RunLabel()
	if len(vm.Egress) > 0 {
		line += " --egress " + joinPorts(vm.Egress, ",")
	}
	return line
}

// joinPorts renders a declared egress port set as one comma-joined run; the
// separator is the caller's (display "22, 443", CLI "22,443").
func joinPorts(ports []int, sep string) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, itoa64(int64(p)))
	}
	return strings.Join(parts, sep)
}

func (s *Server) handleSourceDetail(w http.ResponseWriter, r *http.Request) {
	vm, err := s.sourceDetailVMFor(w, r, r.PathValue("name"))
	if err != nil {
		return // sourceDetailVMFor already wrote the failure response
	}
	s.render(w, r, http.StatusOK, sourceDetail(vm))
}

// sourceDetailVMFor builds the source detail page's render state: the identity
// card, the discovered playbooks, and the launch drawer's prefills. A lookup or
// build failure writes the failure response and returns a non-nil error so the
// caller never double-renders. The run handlers share it for their 422
// re-renders, which only override the drawer's form state on top.
func (s *Server) sourceDetailVMFor(w http.ResponseWriter, r *http.Request, name string) (sourceDetailVM, error) {
	if err := service.ValidSourceName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return sourceDetailVM{}, err
	}
	src, err := s.store.SourceByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no source named "+name, http.StatusNotFound)
			return sourceDetailVM{}, err
		}
		s.fail(w, r, err)
		return sourceDetailVM{}, err
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
		// The operator-set default egress set (Decision 0023), read for
		// everyone; the template renders its line only when non-empty.
		DefaultEgress: src.ExtraEgressPorts,
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
	// No prefill means the first discovered playbook is preselected (SPEC
	// §5.2) — the launcher states one concrete run before any input.
	if vm.Playbook == "" && len(vm.Playbooks) > 0 {
		vm.Playbook = vm.Playbooks[0]
	}
	// The meta names the source's newest playbook job and the Egress field
	// prefills its declared ports (the source's current posture). A lookup
	// failure reads as "no run yet" rather than 500ing the launcher.
	if job, jerr := s.lastPlaybookRun(r.Context(), name); jerr == nil && job != nil {
		vm.LastRunID = job.ID
		vm.LastRunState = string(job.State)
		vm.Egress = job.Args.EgressPorts
	}
	return vm, nil
}

// selectedPlaybook reports whether the discovered path pb is the drawer's
// current playbook choice: the form's posted value on a 422 re-render, else
// the query prefill.
func selectedPlaybook(vm sourceDetailVM, pb string) bool {
	if vm.RunForm != nil {
		return vm.RunForm.Playbook == pb
	}
	return vm.Playbook == pb
}

// playbookRefused and playbookRefusal are the select's meta-line state: the
// form's refusal sentence when the 422 re-render attached one, else at rest.
// They exist so the template never dereferences a nil RunForm.
func (vm sourceDetailVM) playbookRefused() bool {
	return vm.RunForm != nil && vm.RunForm.ErrPlaybook != ""
}

func (vm sourceDetailVM) playbookRefusal() string {
	if vm.RunForm != nil {
		return vm.RunForm.ErrPlaybook
	}
	return ""
}

// runPending and runFormText are the status line's state, nil-form-safe the
// same way the source form's Pending()/ErrFormText() are.
func (vm sourceDetailVM) runPending() bool {
	return vm.RunForm != nil && vm.RunForm.Submitting
}

func (vm sourceDetailVM) runFormText() string {
	if vm.RunForm != nil {
		return vm.RunForm.ErrForm
	}
	return ""
}

// toggleState is a drawer toggle's checked state, kept across a 422 re-render;
// a nil form (at rest) is every toggle off.
func (vm sourceDetailVM) toggleState(name string) bool {
	if vm.RunForm == nil {
		return false
	}
	switch name {
	case "tolerate_unreachable":
		return vm.RunForm.Tolerate
	case "check":
		return vm.RunForm.Check
	case "diff":
		return vm.RunForm.Diff
	case "privileged":
		return vm.RunForm.Privileged
	case "shell":
		return vm.RunForm.Shell
	case "become":
		return vm.RunForm.Become
	case "require_approval":
		return vm.RunForm.RequireApproval
	}
	return false
}

// otherPathSelected reports whether the "other path…" option is the drawer's
// current choice: on a 422 re-render when the form posted the sentinel or a
// free path, else when the query prefill is not a discovered path.
func otherPathSelected(vm sourceDetailVM) bool {
	if vm.RunForm != nil {
		return vm.RunForm.Playbook == playbookOther || vm.RunForm.FreePath != ""
	}
	return !vm.PlaybookKnown()
}

// freePathValue is the free-path input's value: the form's posted value on a
// 422 re-render, else the query prefill when it is not a discovered path.
func freePathValue(vm sourceDetailVM) string {
	if vm.RunForm != nil {
		return vm.RunForm.FreePath
	}
	return vm.FreePrefill()
}

// sourceRunRefusal re-renders the source detail at 422 with the launch drawer
// open on the refused tab, its field and status refusals filled — the JS-off
// path, and the fragment body a JS submit swaps in place. Refusals never flash:
// flash is success-only (write.go).
func (s *Server) sourceRunRefusal(w http.ResponseWriter, r *http.Request, name string, form sourceRunForm) {
	vm, err := s.sourceDetailVMFor(w, r, name)
	if err != nil {
		return
	}
	vm.RunForm = &form
	vm.Launch = true
	vm.Tab = form.Tab
	s.render(w, r, http.StatusUnprocessableEntity, sourceDetail(vm))
}

// lastPlaybookRun is the source's newest playbook job, or nil when no playbook
// has ever run from it. ListJobs returns oldest first, so the newest is the
// last matching row; a job carries the source name as a display fact.
func (s *Server) lastPlaybookRun(ctx context.Context, name string) (*store.Job, error) {
	jobs, err := s.store.ListJobs(ctx, store.JobFilter{Runbook: "playbook"})
	if err != nil {
		return nil, err
	}
	for i := len(jobs) - 1; i >= 0; i-- {
		if jobs[i].Args.Source == name {
			j := jobs[i]
			return &j, nil
		}
	}
	return nil, nil
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
	form := sourceRunForm{
		Tab: "playbook", Playbook: playbook, FreePath: r.PostFormValue("playbook_path"),
		Limit: r.PostFormValue("limit"), Egress: r.PostFormValue("egress"),
		Timeout: r.PostFormValue("timeout"),
		Check:   r.Form.Has("check"), Diff: r.Form.Has("diff"),
		Tolerate: r.Form.Has("tolerate_unreachable"), Privileged: r.Form.Has("privileged"),
	}
	if playbook == "" || playbook == playbookOther {
		playbook = r.PostFormValue("playbook_path")
	}
	// Field refusals re-render the drawer at 422 with its meta lines filled
	// (the forms pattern): the raw validator words go to the log, the field and
	// status lines carry the human sentences.
	if playbook == "" {
		s.log.Info("web: run playbook refused", "err", "empty playbook", "by", ident.token.Name)
		form.ErrPlaybook = sourcePlaybookRefusal
		form.FirstInvalid = "playbook"
		s.sourceRunRefusal(w, r, name, form)
		return
	}
	timeout, err := parseOptionalDuration(r, "timeout")
	if err != nil {
		s.log.Info("web: run playbook refused", "err", "bad timeout", "by", ident.token.Name)
		form.ErrTimeout = sourceTimeoutRefusal
		form.FirstInvalid = "timeout"
		s.sourceRunRefusal(w, r, name, form)
		return
	}

	passthrough, vars, err := service.AnsiblePassthrough(
		r.PostFormValue("limit"), r.PostFormValue("tags"), "",
		r.Form.Has("check"), r.Form.Has("diff"), nil)
	if err != nil {
		s.log.Info("web: run playbook refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrLimit = sourceLimitRefusal
		form.FirstInvalid = "limit"
		s.sourceRunRefusal(w, r, name, form)
		return
	}
	egressPorts, err := parseFormEgress(r)
	if err != nil {
		s.log.Info("web: run playbook refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrEgress = sourceEgressRefusal
		form.FirstInvalid = "egress"
		s.sourceRunRefusal(w, r, name, form)
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
	s.redirectFlash(w, r, "/jobs/"+strconv.FormatInt(res.Job.ID, 10), "job "+strconv.FormatInt(res.Job.ID, 10)+" enqueued")
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
	form := sourceRunForm{
		Tab: "command", Host: host, Command: command,
		Timeout: r.PostFormValue("timeout"),
		Shell:   r.Form.Has("shell"), Become: r.Form.Has("become"),
		RequireApproval: r.Form.Has("require_approval"), Privileged: r.Form.Has("privileged"),
	}
	// Field refusals re-render the drawer at 422 with its meta lines filled
	// (the forms pattern): the raw validator words go to the log.
	if host == "" {
		s.log.Info("web: run command refused", "err", "empty host", "by", ident.token.Name)
		form.ErrHost = sourceHostRefusal
		form.FirstInvalid = "host"
		s.sourceRunRefusal(w, r, name, form)
		return
	}
	if command == "" {
		s.log.Info("web: run command refused", "err", "empty command", "by", ident.token.Name)
		form.ErrCommand = sourceCommandRefusal
		form.FirstInvalid = "command"
		s.sourceRunRefusal(w, r, name, form)
		return
	}
	module := "command"
	if r.Form.Has("shell") {
		module = "shell"
	}
	timeout, err := parseOptionalDuration(r, "timeout")
	if err != nil {
		s.log.Info("web: run command refused", "err", "bad timeout", "by", ident.token.Name)
		form.ErrTimeout = sourceTimeoutRefusal
		form.FirstInvalid = "timeout"
		s.sourceRunRefusal(w, r, name, form)
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
		// A guard refusal (bad host pattern/module/args) is the operator's
		// input refusal, not a server fault: the drawer re-renders at 422 with
		// the typed host/command kept and the status line naming the data's
		// fate. RunAdhoc returns those as plain errors with no store sentinel
		// to match on.
		s.log.Info("web: run command refused", "err", err.Error(), "by", ident.token.Name)
		form.ErrForm = sourceRunSubmitRefusal
		s.sourceRunRefusal(w, r, name, form)
		return
	}

	if res.Gated {
		a := res.Approval
		s.log.Info("web: run command gated", "job", a.JobID, "approval", a.ApprovalID, "source", name, "by", ident.token.Name)
		s.redirectFlash(w, r, "/approvals",
			"job "+strconv.FormatInt(a.JobID, 10)+" gated; approval "+strconv.FormatInt(a.ApprovalID, 10)+" pending")
		return
	}
	s.log.Info("web: run command", "job", res.Run.Job.ID, "source", name, "host", host, "by", ident.token.Name)
	s.redirectFlash(w, r, "/jobs/"+strconv.FormatInt(res.Run.Job.ID, 10),
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
