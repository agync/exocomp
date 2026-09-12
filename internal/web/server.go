package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a-h/templ"

	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/litestream"
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
	// Ntfy drives the ntfy leg's judgment and test push (SPEC §2). It is the
	// daemon's seam over the sealed ntfy secrets; a nil Ntfy (a surface built
	// without one) degrades the leg to presence-only and answers the Test verb
	// with "not available here".
	Ntfy NtfyDriver
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
	// BasePath is the origin sub-path the surface is served under (e.g.
	// "/exocomp"). Empty serves at the origin root — the default. The mux
	// strips it inbound (http.StripPrefix) and every outbound URL and
	// redirect re-prefixes it; see url().
	BasePath string
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
	ntfy        NtfyDriver
	sealer      Sealer
	insecure    bool
	fingerprint string
	version     string
	dataDir     string
	// base is the normalized --base-path the surface is served under; "" is
	// the origin root. Inbound requests have it stripped and outbound URLs
	// re-prefix it.
	base  string
	log   *slog.Logger
	clock func() time.Time
	// startedAt is when the daemon process came up — the "since daemon start"
	// anchor the events surface labels its cold-start state with. Set from the
	// injectable clock so a test clock keeps it deterministic.
	startedAt time.Time
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
	// litestreamPresence reports the Litestream sidecar binary's presence and
	// version line. It is a field only so a test can fake an installed or
	// missing binary; production always uses litestream.Presence
	// (internal/litestream/status.go). The card's NO SIDECAR state and the
	// powerline's rpo segment both derive from it.
	litestreamPresence func() (string, string)
	// mcpSrv is the MCP adapter that serves POST /mcp, the one agent MCP
	// surface — HTTP and bearer-scoped, no ambient stdio path (mcp_http.go).
	mcpSrv *mcp.Server
	// approvalStreams counts the attached approvals streams. Each one polls
	// ListApprovals every approvalsStreamPoll against the one-connection store
	// pool shared with the queue and sequencer, so a flood of browser tabs would
	// otherwise multiply that contention without bound. A stream is refused with
	// 503 once maxApprovalStreams are live; it is a guard, not a ledger — a
	// process restart resets it and nothing audits it.
	approvalStreams atomic.Int64
	// approvalStreamsByToken counts attached streams per token id under
	// approvalStreamsMu, so one identity cannot hold every slot (4 per token,
	// 8 total) and a readonly session cannot starve an operator's stream.
	approvalStreamsMu      sync.Mutex
	approvalStreamsByToken map[int64]int
	// jobStreams counts the attached job-event tails, each polling
	// ListJobEvents at the 300ms streamPoll cadence — the same stale-tab
	// contention as approvalStreams, worse per stream, so the same cap shape:
	// refused with 503 once maxJobStreams are live, and a per-token sub-cap so
	// one identity cannot hold every slot.
	jobStreams atomic.Int64
	// jobStreamsByToken counts attached job tails per token id under
	// jobStreamsMu (4 per token, 8 total).
	jobStreamsMu      sync.Mutex
	jobStreamsByToken map[int64]int
	// listApprovals lists the pending approvals for the /approvals render. It
	// is a field only so a test can pin that the page computes its fingerprint
	// from the same one fetch it renders (approvals_stream_test.go); production
	// always uses store.ListApprovals.
	listApprovals func(ctx context.Context, pendingOnly bool) ([]store.Approval, error)
	// planArtifactsFor lists a workspace's saved plan artifacts for the
	// /workspaces render. It is a field only so a test can count how often the
	// readonly render calls it (workspace_write_test.go); production always
	// uses store.ListPlanArtifacts.
	planArtifactsFor func(ctx context.Context, runbook, kind string, limit int) ([]store.Artifact, error)
	// heartbeat is the idle-keepalive interval of both live tails (approvals
	// and job). A field so a test can shrink the 30s production cadence into a
	// runnable window.
	heartbeat time.Duration
	// streamStop is closed once at shutdown to end every attached stream —
	// approvals and job tails alike — so a SIGINT does not burn the whole
	// grace period on idle sockets. It closes through streamStopOnce because
	// surface shutdown can be reached twice (tests, error paths).
	streamStop     chan struct{}
	streamStopOnce sync.Once
}

// New returns a Server ready to be mounted.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("web: no store")
	}
	if opts.Deriver == nil {
		return nil, errors.New("web: no deriver")
	}
	// The base path is a path, not a scheme or host: it must begin with a
	// single slash and carry no trailing slash, so `/exocomp/` and `/exocomp`
	// name the same origin and a bare "/" collapses to the root origin.
	base := opts.BasePath
	for strings.HasSuffix(base, "/") {
		base = strings.TrimSuffix(base, "/")
	}
	if base != "" && base[0] != '/' {
		return nil, errors.New("web: base path must begin with /: " + opts.BasePath)
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
		store:                  opts.Store,
		deriver:                opts.Deriver,
		decider:                opts.Decider,
		plans:                  opts.Plans,
		maint:                  opts.Maint,
		ntfy:                   opts.Ntfy,
		sealer:                 opts.Sealer,
		insecure:               opts.Insecure,
		fingerprint:            opts.Fingerprint,
		version:                opts.Version,
		dataDir:                opts.DataDir,
		base:                   base,
		log:                    log,
		clock:                  clock,
		heartbeat:              approvalsStreamHeartbeat,
		streamStop:             make(chan struct{}),
		kdf:                    newKDFGate(),
		fails:                  newFailGate(),
		verify:                 verifySecret,
		approvalStreamsByToken: make(map[int64]int),
		jobStreamsByToken:      make(map[int64]int),
	}
	srv.planArtifactsFor = srv.store.ListPlanArtifacts
	srv.listApprovals = srv.store.ListApprovals
	srv.probe = func(ctx context.Context) wire.HealthView {
		return health.Probe(ctx, srv.dataDir, srv.healthOpenStore)
	}
	srv.litestreamPresence = litestream.Presence
	srv.mcpSrv = mcp.New(opts.Store, opts.DataDir, clock)
	srv.mcpSrv.SetLogger(log)
	// This process already holds the envelope, so a gated apply through the MCP
	// face derives its approval capability from it rather than unwrapping the
	// keyring a second time.
	srv.mcpSrv.UseDeriver(opts.Deriver)
	// The daemon start time is the "since daemon start {hh:mm}" the events
	// screen labels its cold-start state with; it comes from the injected clock
	// so a test clock keeps it deterministic.
	srv.startedAt = srv.clock()
	return srv, nil
}

func (s *Server) now() time.Time { return s.clock().UTC() }

// basePath is the normalized origin sub-path the surface is served under: ""
// at the root origin, "/exocomp" under a prefix. It is the seam L-B2's
// template sweep re-prefixes its URLs from.
func (s *Server) basePath() string { return s.base }

// url re-prefixes a root-absolute path with the surface's base path, so every
// redirect and Go-built URL lands inside the prefix. Callers pass the STRIPPED
// root path (the shape handlers see); the prefix is added exactly once.
func (s *Server) url(path string) string { return urlWithBase(s.base, path) }

// urlWithBase joins a root-absolute path onto a base path. Empty base keeps
// the root origin; a path that already carries the base is left alone, so the
// helper never double-prefixes.
func urlWithBase(base, path string) string {
	if base == "" {
		return path
	}
	if strings.HasPrefix(path, base) {
		return path
	}
	return base + path
}

// StopStreams ends every attached approvals stream. It is idempotent: surface
// shutdown can be reached twice (tests, error paths), and a second close of the
// channel would panic.
func (s *Server) StopStreams() { s.streamStopOnce.Do(func() { close(s.streamStop) }) }

//go:embed tokens.css app.css lift.css
var assetFS embed.FS

// assetVer is a short content hash of the embedded stylesheets, appended to
// their URLs as a cache-busting query. The assets carry max-age (cacheControl),
// so without this a browser keeps a prior build's app.css until it expires —
// rendering fresh markup and inline JS against stale CSS. Hashing the content
// means the query changes exactly when the CSS does, so a new build is fetched
// at once while an unchanged one still caches.
var assetVer = func() string {
	h := sha256.New()
	for _, name := range []string{"tokens.css", "app.css", "lift.css"} {
		b, err := assetFS.ReadFile(name)
		if err != nil {
			return "dev"
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

// AssetURL renders an embedded asset path with the cache-busting version query,
// so templates never hardcode the bare path.
func AssetURL(path string) string { return path + "?v=" + assetVer }

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
	mux.HandleFunc("GET /jobs/{id}/raw", s.requireAuth(s.handleJobRaw))
	mux.HandleFunc("GET /approvals", s.requireAuth(s.handleApprovals))
	mux.HandleFunc("GET /approvals/stream", s.requireAuth(s.handleApprovalStream))
	mux.HandleFunc("GET /approvals/{id}", s.requireAuth(s.handleApprovalSurface))
	mux.HandleFunc("POST /approvals/{id}/decide", s.requireAuth(s.handleDecide))
	mux.HandleFunc("POST /kill", s.requireAuth(s.handleKill))
	mux.HandleFunc("GET /events", s.requireAuth(s.handleEvents))
	mux.HandleFunc("GET /events/{id}", s.requireAuth(s.handleEventTrace))
	mux.HandleFunc("GET /system", s.requireAuth(s.handleSystem))
	mux.HandleFunc("GET /system/deadman", s.requireAuth(s.handleDeadman))
	mux.HandleFunc("GET /system/ntfy", s.requireAuth(s.handleNtfy))
	mux.HandleFunc("GET /system/litestream", s.requireAuth(s.handleLitestream))
	mux.HandleFunc("GET /system/backups", s.requireAuth(s.handleBackups))

	// Post-v1 read surfaces (registration and mutation stay CLI/local-auth).
	mux.HandleFunc("GET /sources", s.requireAuth(s.handleSources))
	mux.HandleFunc("GET /schedules", s.requireAuth(s.handleSchedules))
	mux.HandleFunc("GET /netgroups", s.requireAuth(s.handleNetgroups))
	mux.HandleFunc("GET /checks", s.requireAuth(s.handleChecks))
	mux.HandleFunc("GET /workflows", s.requireAuth(s.handleWorkflows))
	mux.HandleFunc("GET /workflows/{id}", s.requireAuth(s.handleWorkflowDetail))
	mux.HandleFunc("GET /policies", s.requireAuth(s.handlePolicies))
	mux.HandleFunc("GET /secrets", s.requireAuth(s.handleSecrets))
	mux.HandleFunc("GET /runners", s.requireAuth(s.handleRunners))
	mux.HandleFunc("GET /rules", s.requireAuth(s.handleRules))

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

	// The base path is stripped here, at the surface edge: handlers route and
	// build URLs against the stripped path, and url() re-prefixes what leaves.
	// StripPrefix with an empty prefix is the identity handler, so the default
	// (no --base-path) serves byte-identically to an unwrapped mux. baseRoot
	// sits outside the strip so it sees the ORIGINAL request: a bare /exocomp
	// is folded onto /exocomp/ before the strip yields "/".
	return baseRoot(s.base, http.StripPrefix(s.base, securityHeaders(mux)))
}

// baseRoot folds a request for the bare base path (https://origin/exocomp with
// no trailing slash) onto the base path plus a slash, so StripPrefix yields "/"
// and the surface's root route serves it exactly as it serves the origin root.
// A bare base otherwise strips to an empty path that matches no mux pattern.
func baseRoot(base string, next http.Handler) http.Handler {
	if base == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == base {
			r2 := new(http.Request)
			*r2 = *r
			r2.URL = new(url.URL)
			*r2.URL = *r.URL
			r2.URL.Path = base + "/"
			r2.URL.RawPath = ""
			r = r2
		}
		next.ServeHTTP(w, r)
	})
}

// render writes a templ component as an HTML response.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	// One-shot flash: any render consumes the cookie so a refresh never resows
	// it. The layout draws p.Flash (set by pageFor from the same cookie) above
	// the page content.
	clearFlashCookie(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := c.Render(r.Context(), w); err != nil {
		s.log.Error("web: render", "path", r.URL.Path, "err", err)
	}
}

// pageFor builds the chrome for an authenticated request: who is signed in and
// the footer signal line.
func (s *Server) pageFor(r *http.Request, title, nav string) page {
	p := page{Title: title, Nav: nav, Insecure: s.insecure, Signal: s.signal(r.Context()), Base: s.base}
	p.Badges = s.navBadges(r.Context())
	// Flash comes from the redirect cookie (server-set only), never a URL
	// parameter — a deep link can name no message it owns.
	if c, err := r.Cookie(flashCookie); err == nil {
		p.Flash = c.Value
	}
	if id, ok := identityOf(r.Context()); ok {
		p.Who = id.token.Name
		p.Scope = id.token.Scope
	}
	return p
}

// navBadges is the nav's actionable state counts (F7): how many probe targets
// are down (Checks), how many sources are never-synced, and how many approvals
// are pending. A count is the number of things needing a look, never a pill.
func (s *Server) navBadges(ctx context.Context) navBadges {
	var b navBadges
	if n, err := s.store.CountCheckStatesByStatus(ctx, string(store.CheckDown)); err == nil {
		b.ChecksDown = n
	}
	if srcs, err := s.store.ListSources(ctx); err == nil {
		for _, src := range srcs {
			if src.LastSyncSHA == "" {
				b.Sources++
			}
		}
	}
	if apps, err := s.store.ListApprovals(ctx, true); err == nil {
		b.Approvals = len(apps)
	}
	return b
}

// securityHeaders sets the headers the surface should always carry. The CSP is
// deliberately strict: the page's only script is its own inline theme/stream
// helpers, all same-origin, plus the Google Fonts
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
