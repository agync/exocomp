package web

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"

	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/mcp"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// Deriver is the slice of the secrets envelope the web surface needs: the
// session-cookie key and the per-job approval capability. Nothing here holds a
// secret of its own.
type Deriver interface {
	Derive(purpose, id string) []byte
}

// Decider resolves a human's decision. It is the reactive engine in production;
// an interface so the surface is testable without one. A nil Decider means the
// web surface can show approvals but not decide them.
type Decider interface {
	Decide(ctx context.Context, approvalID int64, decision, token, via string) (reactive.Decision, error)
}

// PlanOpener reads a sealed plan artifact through the envelope. The approval
// surface renders plan diffs through it and NEVER reads ciphertext raw. A nil
// opener means plan diffs are not shown (the decision still stands on its
// provenance).
type PlanOpener interface {
	OpenSealedFile(path string) ([]byte, error)
}

// Options configures a Server.
type Options struct {
	Store   *store.Store
	Deriver Deriver
	Decider Decider
	Plans   PlanOpener
	// Maint drives the drain/cordon maintenance action. It is the running
	// daemon's control seam; a nil Maint (a web-only process that runs no jobs
	// and no scheduler) makes the maintenance routes answer "not available here".
	Maint Maintenance
	// Sealer seals plaintext into the envelope for the sensitive write surface
	// (secrets add/rotate, a git source's deploy key). A nil Sealer (a web-only
	// process holding no envelope) fail-closes those routes to a 503.
	Sealer   Sealer
	Insecure bool
	// Fingerprint is the LAN certificate's SHA-256, shown on the login screen
	// for trust-on-first-use pinning. Empty under --insecure-http.
	Fingerprint string
	// Version is the string the footer signal line prints.
	Version string
	// DataDir is statfs'd for the footer's disk-headroom signal. Empty leaves
	// the disk field blank.
	DataDir string
	Logger  *slog.Logger
	Clock   func() time.Time
}

// Server is the browser UI and phone approval surface. It reads the store
// directly rather than through the loopback API, so there is one authentication
// story (the LAN token), not two.
type Server struct {
	store       *store.Store
	deriver     Deriver
	decider     Decider
	plans       PlanOpener
	maint       Maintenance
	sealer      Sealer
	insecure    bool
	fingerprint string
	version     string
	dataDir     string
	log         *slog.Logger
	clock       func() time.Time
	// kdf bounds concurrent argon2id evaluations on the pre-auth path so a
	// flood cannot amplify the KDF's memory cost.
	kdf *kdfGate
	// fails counts consecutive authentication failures per source and per token
	// row and backs a guessing run off.
	fails *failGate
	// verify checks a presented secret against a stored argon2id hash. It is a
	// field only so a test can observe the KDF's concurrency; production always
	// uses verifySecret.
	verify func(secret, phc string) (bool, error)
	// probe runs the doctor subsystem probes behind the health cache. It is a
	// field only so a test can count how often it actually runs; production
	// always uses internal/health.Probe.
	probe  func(ctx context.Context) wire.HealthView
	health healthCache
	// mcpSrv is the MCP adapter that serves POST /mcp, the one agent MCP
	// surface — HTTP and bearer-scoped, no ambient stdio path (mcp_http.go).
	mcpSrv *mcp.Server
}

// New returns a Server ready to be mounted.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("web: no store")
	}
	if opts.Deriver == nil {
		return nil, errors.New("web: no deriver")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	srv := &Server{
		store:       opts.Store,
		deriver:     opts.Deriver,
		decider:     opts.Decider,
		plans:       opts.Plans,
		maint:       opts.Maint,
		sealer:      opts.Sealer,
		insecure:    opts.Insecure,
		fingerprint: opts.Fingerprint,
		version:     opts.Version,
		dataDir:     opts.DataDir,
		log:         log,
		clock:       clock,
		kdf:         newKDFGate(),
		fails:       newFailGate(),
		verify:      verifySecret,
	}
	srv.probe = func(ctx context.Context) wire.HealthView {
		return health.Probe(ctx, srv.dataDir, srv.healthOpenStore)
	}
	srv.mcpSrv = mcp.New(opts.Store, opts.DataDir, clock)
	srv.mcpSrv.SetLogger(log)
	// This process already holds the envelope, so a gated apply through the MCP
	// face derives its approval capability from it rather than unwrapping the
	// keyring a second time.
	srv.mcpSrv.UseDeriver(opts.Deriver)
	return srv, nil
}

func (s *Server) now() time.Time { return s.clock().UTC() }

//go:embed tokens.css app.css htmx.min.js
var assetFS embed.FS

// Handler returns the routed surface. Login and assets are open; everything
// else is behind requireAuth. It is a plain http.Handler, so the daemon can
// mount it on the one listener and a test can drive it without a socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /assets/", http.StripPrefix("/assets/", cacheControl(http.FileServer(http.FS(assetFS)))))

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.HandleFunc("GET /{$}", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("GET /jobs", s.requireAuth(s.handleJobs))
	mux.HandleFunc("GET /jobs/{id}", s.requireAuth(s.handleJobDetail))
	mux.HandleFunc("GET /jobs/{id}/stream", s.requireAuth(s.handleJobStream))
	mux.HandleFunc("GET /approvals", s.requireAuth(s.handleApprovals))
	mux.HandleFunc("GET /approvals/{id}", s.requireAuth(s.handleApprovalSurface))
	mux.HandleFunc("POST /approvals/{id}/decide", s.requireAuth(s.handleDecide))
	mux.HandleFunc("POST /kill", s.requireAuth(s.handleKill))
	mux.HandleFunc("GET /events", s.requireAuth(s.handleEvents))
	mux.HandleFunc("GET /events/{id}", s.requireAuth(s.handleEventTrace))

	// Post-v1 read surfaces (registration and mutation stay CLI/local-auth).
	mux.HandleFunc("GET /sources", s.requireAuth(s.handleSources))
	mux.HandleFunc("GET /schedules", s.requireAuth(s.handleSchedules))
	mux.HandleFunc("GET /workflows", s.requireAuth(s.handleWorkflows))
	mux.HandleFunc("GET /workflows/{id}", s.requireAuth(s.handleWorkflowDetail))
	mux.HandleFunc("GET /policies", s.requireAuth(s.handlePolicies))
	mux.HandleFunc("GET /secrets", s.requireAuth(s.handleSecrets))
	mux.HandleFunc("GET /runners", s.requireAuth(s.handleRunners))

	// Browser write surface: each resource's mutating form POSTs (cookie-session,
	// operator-scoped, same-origin) register through here so this file stays the
	// read-surface map. Sensitive writes re-auth; see write.go.
	s.registerWriteUI(mux)

	// The operator HTTP/JSON API shares this listener and its auth; registerAPI
	// adds the /api/v1 routes (agent-interface §3.4).
	s.registerAPI(mux)

	// The MCP transport rides the same listener and the same bearer+scope guard
	// (agent-interface §3.5); registerMCP adds POST /mcp.
	s.registerMCP(mux)

	return securityHeaders(mux)
}

// render writes a templ component as an HTML response.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := c.Render(r.Context(), w); err != nil {
		s.log.Error("web: render", "path", r.URL.Path, "err", err)
	}
}

// pageFor builds the chrome for an authenticated request: who is signed in and
// the footer signal line.
func (s *Server) pageFor(r *http.Request, title, nav string) page {
	p := page{Title: title, Nav: nav, Insecure: s.insecure, Signal: s.signal(r.Context())}
	p.Flash = r.URL.Query().Get("flash")
	if id, ok := identityOf(r.Context()); ok {
		p.Who = id.token.Name
		p.Scope = id.token.Scope
	}
	return p
}

// securityHeaders sets the headers the surface should always carry. The CSP is
// deliberately strict: the page's only script is its own inline theme/stream
// helpers and the vendored htmx, all same-origin, plus the Google Fonts
// stylesheet the design tokens name.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
				"font-src https://fonts.gstatic.com; img-src 'self' data:; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}
