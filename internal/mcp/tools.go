package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// codeInvalidArgument is the taxonomy code for a malformed tool argument (a
// missing name, a bad duration). It has no store sentinel — it is a boundary
// condition — so it is named here, as the HTTP API names it too.
const codeInvalidArgument = "invalid_argument"

// argError is a boundary error carrying the invalid_argument taxonomy code, so a
// bad argument reaches the agent as a branchable code, not a bare sentence.
type argError string

func (e argError) Error() string { return string(e) }

func invalidArg(msg string) error { return argError(msg) }

// toolFunc runs one tool over decoded arguments and returns the wire kind, the
// projection, and any error. The dispatcher marshals all three into the MCP
// result — success or a wire error envelope — so every tool's bytes come from
// the one wire marshaller.
type toolFunc func(ctx context.Context, args json.RawMessage) (string, any, error)

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

// tools is the catalog: read tools (R), the cleanly-serviceable write tools (W),
// and the two approval-gated decision tools (A). run_playbook is DEFERRED — a
// server-side run must resolve an inventory name to a path and a --source
// playbook onto its checkout, and those resolvers are still CLI-resident
// (resolveInventoryArg/sourcePlaybook); exposing the tool without them would let
// an agent's names miss their paths. The other mutating verbs (sync_source,
// backup_run, schedule_*, secret_*) are out of this lane's scope by the task.
func (s *Server) tools() []tool {
	return []tool{
		// --- read ---
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

		// --- write (enqueue through the service) ---
		{"workspace_plan", "Enqueue a tofu plan for a workspace; returns the job id.",
			object(map[string]any{
				"name":            strProp("the workspace name"),
				"run_secrets":     strProp("run-secret binding name"),
				"timeout":         strProp("Go duration, e.g. 30m"),
				"idempotency_key": strProp("dedup key so a retry does not double-enqueue"),
			}, "name"), s.runWorkspacePlan},
		{"workspace_apply", "Enqueue a tofu apply; a require-approval saved plan returns a pending approval.",
			object(map[string]any{
				"name":            strProp("the workspace name"),
				"plan":            strProp("saved plan artifact id (omit for a fresh apply)"),
				"run_secrets":     strProp("run-secret binding name"),
				"timeout":         strProp("Go duration, e.g. 30m"),
				"idempotency_key": strProp("dedup key so a retry does not double-enqueue"),
			}, "name"), s.runWorkspaceApply},

		// --- approve (reactive decide seam) ---
		{"approve_job", "Approve a pending approval; releases its gated job. Decided server-side.",
			object(map[string]any{"approval_id": intProp("the approval id")}, "approval_id"), s.runApprove},
		{"reject_job", "Reject a pending approval; withdraws its gated job. Decided server-side.",
			object(map[string]any{"approval_id": intProp("the approval id")}, "approval_id"), s.runReject},
	}
}

// handleToolsList projects the catalog to the MCP tools/list shape.
func (s *Server) handleToolsList() map[string]any {
	list := s.tools()
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
func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p callParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params"}
	}
	var run toolFunc
	for _, t := range s.tools() {
		if t.name == p.Name {
			run = t.run
			break
		}
	}
	if run == nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
	}
	kind, data, err := run(ctx, p.Arguments)
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
// or an error envelope mapped onto the taxonomy (a boundary argError keeps its
// invalid_argument code). It is the one place tool and resource output is
// marshalled, so their bytes are the CLI's and the API's bytes.
func envelopeText(kind string, data any, err error) (string, bool) {
	if err != nil {
		var e wire.Error
		if ae, ok := err.(argError); ok {
			e = wire.NewError(codeInvalidArgument, ae.Error())
		} else {
			e = wire.ErrorFor(err)
		}
		var buf bytes.Buffer
		_ = wire.WriteErrorValue(&buf, e)
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

// --- argument adapters: decode, then call the shared read/write helper --------

func (s *Server) runListJobs(ctx context.Context, args json.RawMessage) (string, any, error) {
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

func (s *Server) runGetJob(ctx context.Context, args json.RawMessage) (string, any, error) {
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

func (s *Server) runGetJobEvents(ctx context.Context, args json.RawMessage) (string, any, error) {
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

func (s *Server) runListWorkspaces(ctx context.Context, _ json.RawMessage) (string, any, error) {
	return s.readWorkspaces(ctx)
}

func (s *Server) runGetWorkspace(ctx context.Context, args json.RawMessage) (string, any, error) {
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

func (s *Server) runGetDrift(ctx context.Context, args json.RawMessage) (string, any, error) {
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

func (s *Server) runListApprovals(ctx context.Context, args json.RawMessage) (string, any, error) {
	var a struct {
		All bool `json:"all"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	return s.readApprovals(ctx, !a.All)
}

func (s *Server) runListSources(ctx context.Context, _ json.RawMessage) (string, any, error) {
	return s.readSources(ctx)
}

func (s *Server) runListSchedules(ctx context.Context, _ json.RawMessage) (string, any, error) {
	return s.readSchedules(ctx)
}

func (s *Server) runListRules(ctx context.Context, _ json.RawMessage) (string, any, error) {
	return s.readRules(ctx)
}

func (s *Server) runListEvents(ctx context.Context, args json.RawMessage) (string, any, error) {
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

func (s *Server) runExplainEvent(ctx context.Context, args json.RawMessage) (string, any, error) {
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

func (s *Server) runListSecrets(ctx context.Context, _ json.RawMessage) (string, any, error) {
	return s.readSecrets(ctx)
}

func (s *Server) runListBackups(ctx context.Context, _ json.RawMessage) (string, any, error) {
	return s.readBackups(ctx)
}

func (s *Server) runHealth(ctx context.Context, _ json.RawMessage) (string, any, error) {
	return s.readHealth(ctx)
}

func (s *Server) runWorkspacePlan(ctx context.Context, args json.RawMessage) (string, any, error) {
	var a planWriteArgs
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	return s.toolWorkspacePlan(ctx, a)
}

func (s *Server) runWorkspaceApply(ctx context.Context, args json.RawMessage) (string, any, error) {
	var a planWriteArgs
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	return s.toolWorkspaceApply(ctx, a)
}

func (s *Server) runApprove(ctx context.Context, args json.RawMessage) (string, any, error) {
	var a decideArgs
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	return s.decide(ctx, reactive.DecisionApprove, a)
}

func (s *Server) runReject(ctx context.Context, args json.RawMessage) (string, any, error) {
	var a decideArgs
	if err := decodeArgs(args, &a); err != nil {
		return "", nil, err
	}
	return s.decide(ctx, reactive.DecisionReject, a)
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
