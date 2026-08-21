package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TokenPurpose is the HKDF purpose the per-workspace bearer is derived under.
// The workspace name is the derivation id, so a token opens exactly one
// workspace's state and nothing else.
const TokenPurpose = "tf-backend"

// StatePrefix is the path the OpenTofu HTTP backend talks to; the workspace
// name is the final segment.
const StatePrefix = "/api/v1/tf/state/"

// TokenDeriver is the part of the secrets envelope the backend needs.
type TokenDeriver interface {
	Derive(purpose, id string) []byte
}

// WorkspaceToken returns the bearer that authenticates state requests for one
// workspace. It is deterministic: the driver, the server, and a recovery-time
// operator with the envelope all compute the same string.
func WorkspaceToken(d TokenDeriver, workspace string) string {
	return base64.StdEncoding.EncodeToString(d.Derive(TokenPurpose, workspace))
}

// Options configures a Server.
type Options struct {
	// Store holds the workspaces, their state versions, and their locks.
	Store *store.Store
	// Tokens derives the per-workspace bearer.
	Tokens TokenDeriver
	// Port is the loopback port to bind. Zero takes an ephemeral one, which
	// Addr reports once the server has started.
	Port int
	// Logger receives serving failures. Default slog.Default().
	Logger *slog.Logger
}

// Server is the OpenTofu HTTP state backend. It binds loopback only: the
// backend is reachable by this host's own job subprocesses and nothing else,
// which is what lets a derived bearer stand in for a full authentication
// stack (ADR 0004).
type Server struct {
	store  *store.Store
	tokens TokenDeriver
	log    *slog.Logger
	port   int

	http *http.Server
	ln   net.Listener

	mu   sync.Mutex
	jobs map[string]int64
}

// New returns a server ready to Start.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("api: no store")
	}
	if opts.Tokens == nil {
		return nil, errors.New("api: no token deriver")
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return nil, fmt.Errorf("api: port %d out of range", opts.Port)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		store:  opts.Store,
		tokens: opts.Tokens,
		log:    log,
		port:   opts.Port,
		jobs:   map[string]int64{},
	}, nil
}

// Handler returns the routes, for tests that drive them without a listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc(StatePrefix+"{workspace}", s.handleState)
	return mux
}

// Start binds the loopback port and serves in the background.
func (s *Server) Start() error {
	if s.ln != nil {
		return errors.New("api: already started")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(s.port)))
	if err != nil {
		return fmt.Errorf("api: listen: %w", err)
	}
	s.ln = ln
	s.http = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("state backend stopped", "err", err)
		}
	}()
	return nil
}

// Addr reports the bound address, empty before Start.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// StateURL is the backend address for one workspace, as the generated backend
// configuration writes it.
func StateURL(addr, workspace string) string {
	return "http://" + addr + StatePrefix + url.PathEscape(workspace)
}

// Shutdown stops serving and waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("api: shutdown: %w", err)
	}
	return nil
}

// BindJob attributes the workspace's next state pushes and locks to a job, so
// a stale lock names the job that must be dead before it is broken. A
// workspace with no bound job stores NULL, which is what a push arriving at a
// long-running server without a local job looks like.
func (s *Server) BindJob(workspace string, jobID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[workspace] = jobID
}

// ReleaseJob drops the attribution BindJob made.
func (s *Server) ReleaseJob(workspace string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, workspace)
}

func (s *Server) jobFor(workspace string) *int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.jobs[workspace]
	if !ok {
		return nil
	}
	return &id
}
