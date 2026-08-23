package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/version"
)

// protocolVersion is the MCP revision this server speaks. initialize echoes it.
const protocolVersion = "2024-11-05"

// serverName is the MCP server identity a harness sees.
const serverName = "exocomp"

// serverInstructions is the MCP spec's optional top-level guidance string,
// returned once at handshake. It exists to head off one specific
// misreading: "event" names two unrelated id-spaces in this catalog, and a
// cold agent that has not yet read every tool description tends to conflate
// them.
const serverInstructions = "exocomp MCP server: jobs, workspaces, secrets, and " +
	"reactive triggers over the same store the CLI and HTTP API use. 'Event' " +
	"names two unrelated things here: get_job_events reads a JOB's own " +
	"execution log (its seq-cursor id-space, keyed by job id); " +
	"list_events/explain_event read INBOUND trigger events (a separate " +
	"id-space) that may enqueue jobs. Do not pass a job id to explain_event " +
	"or an event id to get_job_events."

// Server is the MCP adapter. It holds the same open store the CLI's local
// commands hold and the data dir under it; the service verbs and the pure-enqueue
// Enqueuer are built per call over these, exactly as the HTTP write API does.
// It is transport-agnostic: stdio drives it through Serve, a mounted HTTP
// transport through HandleMessage, and the two differ only in the Caller they
// hand to the one dispatch.
type Server struct {
	store   *store.Store
	dataDir string
	now     func() time.Time
	log     *slog.Logger

	// newEnqueuer builds the pure-enqueue Enqueuer a write tool routes through.
	// It is a field so a test can inject a real queue over its own store without
	// this package reaching for process state.
	newEnqueuer func() service.Enqueuer

	// openEnvelope lazily opens the secrets envelope that mints approval-decision
	// capabilities (reactive.Deriver). Only the gated apply and approve/reject
	// need it; a plan and a fresh apply do not, so it is never opened for them.
	// Injectable so a test can supply a deriver without a real keyring.
	openEnvelope func() (reactive.Deriver, func(), error)
}

// New returns a Server over an already-open store and its data dir. A nil clock
// defaults to time.Now in UTC.
func New(st *store.Store, dataDir string, now func() time.Time) *Server {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	s := &Server{store: st, dataDir: dataDir, now: now, log: slog.Default()}
	s.newEnqueuer = func() service.Enqueuer {
		return pureEnqueuer{q: queue.New(st, noRunDriver{}, queue.Options{Workers: 1})}
	}
	s.openEnvelope = func() (reactive.Deriver, func(), error) {
		env, err := secrets.Open(dataDir)
		if err != nil {
			return nil, nil, err
		}
		return env, func() { _ = env.Close() }, nil
	}
	return s
}

// SetLogger overrides the diagnostic logger (it never carries payload values).
func (s *Server) SetLogger(l *slog.Logger) {
	if l != nil {
		s.log = l
	}
}

// Serve runs the MCP protocol over a stdio pair until in is exhausted or ctx is
// cancelled. It is the one entry point `exocomp mcp` calls, and it runs as the
// LocalCaller: the local operator's authority, with no approve capability.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	return s.serve(ctx, LocalCaller(), in, out)
}

// UseDeriver supplies an already-open approval-capability deriver instead of
// opening the secrets envelope per gated apply. A transport mounted inside a
// process that already holds the envelope (the web listener) passes its own, so
// the daemon does not unwrap the keyring a second time; stdio opens its own.
func (s *Server) UseDeriver(d reactive.Deriver) {
	if d == nil {
		return
	}
	s.openEnvelope = func() (reactive.Deriver, func(), error) { return d, func() {}, nil }
}

// service builds the operational core over the server's store, data dir, and
// clock — the same verbs the CLI drives, differing only in the Enqueuer.
func (s *Server) service() *service.Service {
	return service.New(s.store, s.dataDir, s.now)
}

// dispatch routes one JSON-RPC method to its handler for one caller. A
// protocol-level failure (unknown method, malformed params) returns an
// *rpcError; a tool/resource failure is carried inside the result as a wire
// error envelope, not here.
//
// Everything past the handshake needs at least the read capability, checked on
// every message rather than once per connection: the transports are stateless
// and re-resolve the credential per request, so a revoked or downgraded token
// stops working on its next call instead of at the end of its session (the same
// property the SSE tail gained in finding AI-10).
func (s *Server) dispatch(ctx context.Context, c Caller, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.handleInitialize(), nil
	case "ping":
		return struct{}{}, nil
	}
	if !c.allows(capRead) {
		return nil, &rpcError{Code: codeInvalidRequest, Message: "this caller's scope may not read"}
	}
	switch method {
	case "tools/list":
		return s.handleToolsList(c), nil
	case "tools/call":
		return s.handleToolsCall(ctx, c, params)
	case "resources/list":
		return s.handleResourcesList(), nil
	case "resources/templates/list":
		return s.handleResourceTemplatesList(), nil
	case "resources/read":
		return s.handleResourcesRead(ctx, params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
	}
}

// handleInitialize advertises the protocol version, the tool and resource
// capabilities, and the server identity.
func (s *Server) handleInitialize() map[string]any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools":     map[string]any{},
			"resources": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": version.String(),
		},
		"instructions": serverInstructions,
	}
}

// pureEnqueuer inserts the queued (or gated) row through the one queue and
// returns at once. Its driver refuses to run: this process only enqueues, and a
// running daemon claims and runs the row. It is the MCP analogue of the HTTP
// write API's apiEnqueuer (internal/web/api_write.go).
type pureEnqueuer struct{ q *queue.Queue }

func (e pureEnqueuer) Enqueue(ctx context.Context, spec queue.Spec) (store.Job, error) {
	return e.q.Enqueue(ctx, spec)
}

func (e pureEnqueuer) EnqueueGated(ctx context.Context, spec queue.Spec) (store.Job, error) {
	return e.q.EnqueueGated(ctx, spec)
}

// noRunDriver is the driver of the enqueue-only queue. Refusing loudly keeps a
// future caller from draining it and racing the daemon for the row.
type noRunDriver struct{}

func (noRunDriver) Run(context.Context, executor.Spec, executor.EventSink) (executor.Result, error) {
	return executor.Result{}, errNoRun
}

var errNoRun = errInternal("mcp: the server only enqueues; the daemon runs the job")

// errInternal is a tiny error type so the package's fixed strings need no
// errors.New calls scattered through it.
type errInternal string

func (e errInternal) Error() string { return string(e) }
