package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// codeInvalidArgument is the taxonomy code for a malformed tool argument (a
// missing name, a bad duration). It has no store sentinel — it is a boundary
// condition — so it is named here, as the HTTP API names it too.
const codeInvalidArgument = "invalid_argument"

// coded is an error that names its own taxonomy code. The boundary conditions
// this face raises — a bad argument, a scope refusal — have no store sentinel
// for wire.ErrorFor to map from, so they carry the code themselves and reach the
// agent as a branchable code rather than a bare sentence.
type coded interface{ WireCode() string }

// argError is a boundary error carrying the invalid_argument taxonomy code.
type argError string

func (e argError) Error() string    { return string(e) }
func (e argError) WireCode() string { return codeInvalidArgument }

func invalidArg(msg string) error { return argError(msg) }

// scopeError is a refusal on the caller's scope: the credential authenticated,
// it just may not do this.
type scopeError string

func (e scopeError) Error() string    { return string(e) }
func (e scopeError) WireCode() string { return wire.CodeForbidden }

func forbidden(msg string) error { return scopeError(msg) }

// toolFunc runs one tool over decoded arguments and returns the wire kind, the
// projection, and any error. The dispatcher marshals all three into the MCP
// result — success or a wire error envelope — so every tool's bytes come from
// the one wire marshaller. The Caller travels with the call so a write records
// who asked and namespaces its idempotency key by that principal.
type toolFunc func(ctx context.Context, c Caller, args json.RawMessage) (string, any, error)

// tool is one catalog entry: its name, a one-line description, its JSON-Schema
// input, and the handler.
type tool struct {
	name        string
	description string
	schema      map[string]any
	run         toolFunc
}

// object builds a JSON-Schema object node. required lists the mandatory keys.
func object(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// tools is the catalog one caller sees. Visibility is scope-gated (ADR 0014 §3):
// a readonly token is advertised only read tools, so an agent is never shown a
// verb its credential cannot exercise — and the same floors are re-checked per
// call in handleToolsCall, because a catalog is a snapshot and a token can be
// revoked or downgraded between two calls.
//
// The W and A columns are DISJOINT by construction (Caller.allows): no scope
// satisfies both, so no principal can raise the approval gate with
// workspace_apply and then release it with approve_job — the human-in-the-loop
// control closed by the actor it exists to stop (findings AI-1 and AI-2).
func (s *Server) tools(c Caller) []tool {
	var out []tool
	if c.allows(capRead) {
		out = append(out, s.readTools()...)
	}
	if c.allows(capWrite) {
		out = append(out, s.writeTools()...)
	}
	if c.allows(capApprove) {
		out = append(out, s.approveTools()...)
	}
	return out
}

// lookup finds a tool by name across the WHOLE catalog and reports the
// capability it needs. Resolving against the full catalog is deliberate: a call
// to a tool the caller cannot see is refused as forbidden, naming the scope it
// needs, rather than answered "unknown tool" — a scope refusal an agent can act
// on, and one that cannot be mistaken for a typo.
func (s *Server) lookup(name string) (tool, capability, bool) {
	for _, set := range []struct {
		tools []tool
		need  capability
	}{
		{s.readTools(), capRead},
		{s.writeTools(), capWrite},
		{s.approveTools(), capApprove},
	} {
		for _, t := range set.tools {
			if t.name == name {
				return t, set.need, true
			}
		}
	}
	return tool{}, capRead, false
}

// readTools is the R column: every one is a values-free projection of state.
func (s *Server) readTools() []tool {
	return []tool{
		{"list_jobs", "List jobs oldest-first; filter by state, runbook, concurrency_key, limit.",
			object(map[string]any{
				"state":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "only jobs in these states"},
				"runbook":         strProp("only jobs for this runbook"),
				"concurrency_key": strProp("only jobs under this concurrency key"),
				"limit":           intProp("how many jobs (0: all)"),
			}), s.runListJobs},
		{"get_job", "Show one job by id.",
			object(map[string]any{"id": intProp("the job id")}, "id"), s.runGetJob},
		{"get_job_events", "Show a job's append-only, already-redacted event log; since skips to after a seq.",
			object(map[string]any{"id": intProp("the job id"), "since": intProp("only events after this seq")}, "id"), s.runGetJobEvents},
		{"list_workspaces", "List tofu workspaces.", object(nil), s.runListWorkspaces},
		{"get_workspace", "Show one workspace by name.",
			object(map[string]any{"name": strProp("the workspace name")}, "name"), s.runGetWorkspace},
		{"get_drift", "Show the newest plan's drift and pending changes for a workspace (values suppressed).",
			object(map[string]any{"name": strProp("the workspace name")}, "name"), s.runGetDrift},
		{"list_approvals", "List approvals; pending-only by default, all=true includes decided.",
			object(map[string]any{"all": boolProp("include already-decided approvals")}), s.runListApprovals},
		{"list_sources", "List configured sources.", object(nil), s.runListSources},
		{"list_schedules", "List schedules.", object(nil), s.runListSchedules},
		{"list_rules", "List reactive rules.", object(nil), s.runListRules},
		{"list_events", "List received events; filter by source, outcome, limit.",
			object(map[string]any{
				"source":  strProp("only events from this source"),
				"outcome": strProp("only events with this outcome"),
				"limit":   intProp("how many events (default 25)"),
			}), s.runListEvents},
		{"explain_event", "Explain why one event ended the way it did (values-free trace).",
			object(map[string]any{"id": intProp("the event id")}, "id"), s.runExplainEvent},
		{"list_secrets", "List secret METADATA only — never a value or ciphertext.", object(nil), s.runListSecrets},
		{"list_backups", "List backup targets.", object(nil), s.runListBackups},
		{"health", "Run the doctor subsystem probes and return a per-subsystem summary.", object(nil), s.runHealth},
	}
}

// writeTools is the W column: each enqueues through the SAME internal/service
// verb the CLI and the HTTP write API call (invariant 5).
//
// run_playbook is DEFERRED — a server-side run must resolve an inventory name to
// a path and a --source playbook onto its checkout, and those resolvers are still
// CLI-resident (resolveInventoryArg/sourcePlaybook); exposing the tool without
// them would let an agent's names miss their paths. The other mutating verbs
// (sync_source, backup_run, schedule_*, secret_*) are out of this lane's scope.
func (s *Server) writeTools() []tool {
	return []tool{
		{"workspace_plan", "Enqueue a tofu plan for a workspace; returns the job id.",
			object(map[string]any{
				"name":            strProp("the workspace name"),
				"run_secrets":     strProp("run-secret binding name"),
				"timeout":         strProp("Go duration, e.g. 30m"),
				"idempotency_key": strProp("dedup key so a retry does not double-enqueue"),
			}, "name"), s.runWorkspacePlan},
		{"workspace_apply", "Apply now. Omit plan and it runs immediately, auto-approved, with NO review gate" +
			" — unless the workspace has an enabled plan policy, in which case a planless apply is refused" +
			" (run workspace_plan first, then apply that plan by id). Pass a saved plan's artifact id" +
			" (a string-encoded integer, e.g. \"42\") to apply that plan instead; a require-approval plan" +
			" then pends for approve_job/reject_job.",
			object(map[string]any{
				"name":            strProp("the workspace name"),
				"plan":            strProp("saved plan artifact id — a string-encoded integer (e.g. \"42\"), not a file path or workspace name"),
				"run_secrets":     strProp("run-secret binding name"),
				"timeout":         strProp("Go duration, e.g. 30m"),
				"idempotency_key": strProp("dedup key so a retry does not double-enqueue"),
			}, "name"), s.runWorkspaceApply},
	}
}

// handleToolsList projects the caller's slice of the catalog to the MCP
// tools/list shape.
func (s *Server) handleToolsList(c Caller) map[string]any {
	list := s.tools(c)
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.schema,
		})
	}
	return map[string]any{"tools": out}
}

// callParams is the tools/call request body.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// handleToolsCall runs one tool and returns the MCP result. A tool failure is
// carried inside the result as a wire error envelope with isError=true — the
// model sees the taxonomy code — never as a JSON-RPC protocol error. An unknown
// tool name is a protocol error (invalid params), because no tool ran.
//
// The caller's scope is re-checked HERE, per call, against the capability the
// named tool needs, and the refusal is returned before the handler runs — so no
// store read, no enqueue, and no envelope open happens for a caller the face
// will refuse (agent-interface §4).
func (s *Server) handleToolsCall(ctx context.Context, c Caller, params json.RawMessage) (any, *rpcError) {
	var p callParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params"}
	}
	t, need, ok := s.lookup(p.Name)
	if !ok {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
	}
	if !c.allows(need) {
		return toolResult("", nil, forbidden(
			"tool "+p.Name+" needs a token of the "+need.scopeFor()+" scope")), nil
	}
	kind, data, err := t.run(ctx, c, p.Arguments)
	return toolResult(kind, data, err), nil
}

// toolResult marshals a handler's outcome into the MCP tool-result shape: one
// text content block holding the wire envelope, and isError set on failure.
func toolResult(kind string, data any, err error) map[string]any {
	text, isErr := envelopeText(kind, data, err)
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// envelopeText renders the wire envelope bytes for a result: a success envelope,
// or an error envelope mapped onto the taxonomy (a boundary error keeps the code
// it names for itself). It is the one place tool and resource output is
// marshalled, so their bytes are the CLI's and the API's bytes.
func envelopeText(kind string, data any, err error) (string, bool) {
	if err != nil {
		var buf bytes.Buffer
		_ = wire.WriteErrorValue(&buf, wireError(err))
		return buf.String(), true
	}
	b, merr := wire.MarshalSuccess(kind, data)
	if merr != nil {
		var buf bytes.Buffer
		_ = wire.WriteErrorValue(&buf, wire.NewError("internal", "marshal result"))
		return buf.String(), true
	}
	return string(b), false
}

// wireError maps a handler error onto the taxonomy: an error that names its own
// code keeps it, anything else goes through the shared sentinel mapping.
func wireError(err error) wire.Error {
	var c coded
	if errors.As(err, &c) {
		return wire.NewError(c.WireCode(), err.Error())
	}
	return wire.ErrorFor(err)
}

// --- argument adapters: decode, then call the shared read/write helper --------

func (s *Server) runListJobs(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a struct {
		State          []string `json:"state"`
		Runbook        string   `json:"runbook"`
		ConcurrencyKey string   `json:"concurrency_key"`
		Limit          int      `json:"limit"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	filter := store.JobFilter{Runbook: a.Runbook, ConcurrencyKey: a.ConcurrencyKey, Limit: a.Limit}
	for _, st := range a.State {
		if !knownJobState(st) {
			return "", nil, invalidArg("state " + strconv.Quote(st) + " is not a job state")
		}
		filter.States = append(filter.States, store.JobState(st))
	}
	return s.readJobs(ctx, filter)
}

func (s *Server) runGetJob(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a struct {
		ID int64 `json:"id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.ID <= 0 {
		return "", nil, invalidArg("id must be a positive integer")
	}
	return s.readJob(ctx, a.ID)
}

func (s *Server) runGetJobEvents(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a struct {
		ID    int64 `json:"id"`
		Since int64 `json:"since"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.ID <= 0 {
		return "", nil, invalidArg("id must be a positive integer")
	}
	return s.readJobEvents(ctx, a.ID, a.Since)
}

func (s *Server) runListWorkspaces(ctx context.Context, _ Caller, _ json.RawMessage) (string, any, error) {
	return s.readWorkspaces(ctx)
}

func (s *Server) runGetWorkspace(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.Name == "" {
		return "", nil, invalidArg("name is required")
	}
	return s.readWorkspace(ctx, a.Name)
}

func (s *Server) runGetDrift(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.Name == "" {
		return "", nil, invalidArg("name is required")
	}
	return s.readDrift(ctx, a.Name)
}

func (s *Server) runListApprovals(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a struct {
		All bool `json:"all"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	return s.readApprovals(ctx, !a.All)
}

func (s *Server) runListSources(ctx context.Context, _ Caller, _ json.RawMessage) (string, any, error) {
	return s.readSources(ctx)
}

func (s *Server) runListSchedules(ctx context.Context, _ Caller, _ json.RawMessage) (string, any, error) {
	return s.readSchedules(ctx)
}

func (s *Server) runListRules(ctx context.Context, _ Caller, _ json.RawMessage) (string, any, error) {
	return s.readRules(ctx)
}

func (s *Server) runListEvents(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a struct {
		Source  string `json:"source"`
		Outcome string `json:"outcome"`
		Limit   int    `json:"limit"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	filter := store.EventFilter{Source: a.Source, Limit: 25}
	if a.Outcome != "" {
		if !knownOutcome(a.Outcome) {
			return "", nil, invalidArg("outcome " + strconv.Quote(a.Outcome) + " is not an event outcome")
		}
		filter.Outcomes = []store.EventOutcome{store.EventOutcome(a.Outcome)}
	}
	if a.Limit > 0 {
		filter.Limit = a.Limit
	}
	return s.readEvents(ctx, filter)
}

func (s *Server) runExplainEvent(ctx context.Context, _ Caller, args json.RawMessage) (string, any, error) {
	var a struct {
		ID int64 `json:"id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	if a.ID <= 0 {
		return "", nil, invalidArg("id must be a positive integer")
	}
	return s.readExplain(ctx, a.ID)
}

func (s *Server) runListSecrets(ctx context.Context, _ Caller, _ json.RawMessage) (string, any, error) {
	return s.readSecrets(ctx)
}

func (s *Server) runListBackups(ctx context.Context, _ Caller, _ json.RawMessage) (string, any, error) {
	return s.readBackups(ctx)
}

func (s *Server) runHealth(ctx context.Context, _ Caller, _ json.RawMessage) (string, any, error) {
	return s.readHealth(ctx)
}

func (s *Server) runWorkspacePlan(ctx context.Context, c Caller, args json.RawMessage) (string, any, error) {
	var a planWriteArgs
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	return s.toolWorkspacePlan(ctx, c, a)
}

func (s *Server) runWorkspaceApply(ctx context.Context, c Caller, args json.RawMessage) (string, any, error) {
	var a planWriteArgs
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	return s.toolWorkspaceApply(ctx, c, a)
}

// decodeArgs unmarshals tool arguments, tolerating an absent arguments object (a
// no-arg tool). A present-but-malformed object is an invalid_argument error.
func decodeArgs(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return invalidArg("arguments are not valid JSON for this tool")
	}
	return nil
}

// knownJobState / knownOutcome mirror the CLI's and the HTTP API's validators,
// using the store constants so an added state cannot silently pass here.
func knownJobState(s string) bool {
	switch store.JobState(s) {
	case store.StateQueued, store.StateAwaitingApproval, store.StateRunning,
		store.StateSucceeded, store.StateFailed, store.StateCancelled,
		store.StateTimedOut, store.StateInterrupted:
		return true
	default:
		return false
	}
}

func knownOutcome(s string) bool {
	switch store.EventOutcome(s) {
	case store.OutcomeIgnored, store.OutcomeMatched, store.OutcomeSuppressed,
		store.OutcomeGated, store.OutcomeFired, store.OutcomeError:
		return true
	default:
		return false
	}
}
