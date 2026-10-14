package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/version"
	"exocomp.dev/exocomp/internal/web"
)

func init() {
	registerCommand(newWebCmd)
}

// webShutdownGrace bounds how long the LAN server waits for in-flight requests
// (an SSE tail among them) once a signal arrives.
const webShutdownGrace = 5 * time.Second

type webOptions struct {
	addr     string
	insecure bool
	basePath string
}

func newWebCmd() *cobra.Command {
	opts := webOptions{addr: "0.0.0.0:8443"}

	cmd := &cobra.Command{
		Use:   "web",
		Short: "Serve the token-gated LAN web UI (Decision 0009)",
		Long: "Serves the browser dashboard and phone approval surface over TLS, " +
			"authenticated by LAN access tokens. A non-loopback bind refuses to start " +
			"without at least one active token. The web process shares the store with a " +
			"running `exocomp serve`: an approval decided here releases the job for that " +
			"daemon to run.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWeb(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.addr, "addr", opts.addr, "address to bind (host:port)")
	cmd.Flags().BoolVar(&opts.insecure, "insecure-http", false,
		"serve cleartext HTTP instead of TLS (loud, never the documented path)")
	cmd.Flags().StringVar(&opts.basePath, "base-path", "",
		"origin sub-path the web surface is served under (e.g. /exocomp); empty serves at the origin root")
	return cmd
}

func runWeb(ctx context.Context, out, warn io.Writer, opts webOptions) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("web: resolve data dir: %w", err)
	}
	// openJobStore, not openStore: this process is a job path — it releases
	// approved jobs — so it sweeps rows abandoned by a dead process on the way
	// in, like every other entry point (the invariant sweep_test guards).
	st, err := openJobStore(ctx, warn)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("web: open envelope (run `exocomp init` first): %w", err)
	}
	defer func() { _ = env.Close() }()

	// A standalone web process releases gated jobs for the daemon to run but
	// runs none itself, so its engine is release-only.
	engine, err := webEngine(st, env)
	if err != nil {
		return err
	}

	// A standalone web process owns no scheduler or job queue, so it exposes no
	// maintenance seam: the daemon that runs jobs is the one to cordon. It does
	// hold the envelope, so the ntfy leg's judgment and test push work here.
	surface, err := startWebSurface(ctx, st, env, dir, engine, nil, newNtfySeam(st, env), opts.basePath, opts.addr, opts.insecure, warn)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "web UI listening on %s://%s\n", surface.scheme, surface.addr)
	if surface.fingerprint != "" {
		_, _ = fmt.Fprintf(out, "certificate SHA-256: %s\n", surface.fingerprint)
	}

	select {
	case <-sigCtx.Done():
		_, _ = fmt.Fprintln(out, "shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webShutdownGrace)
		defer cancel()
		return surface.Shutdown(shutdownCtx)
	case e := <-surface.serveErr:
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	}
}

// webSurface is a running token-gated LAN web listener. It is started by the
// `web` command and, when `serve --web-addr` is set, alongside the loopback
// state backend in the daemon — a separate non-loopback TLS listener for the
// web/token surface, distinct from the loopback state backend (Decision 0009). The
// state backend never requires a LAN token; this surface always does.
type webSurface struct {
	http *http.Server
	// srv is the LAN surface's web handler, retained so Shutdown can end its
	// attached SSE streams before the http grace clock starts ticking (F5).
	srv         *web.Server
	addr        string
	scheme      string
	fingerprint string
	serveErr    <-chan error
}

// Shutdown stops the listener within the caller's deadline. It ends attached
// SSE streams first, so an idle approvals tail does not eat the whole grace
// window before http.Shutdown's drain notices it.
func (w *webSurface) Shutdown(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if w.srv != nil {
		w.srv.StopStreams()
	}
	if w.http == nil {
		return nil
	}
	if err := w.http.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startWebSurface builds and starts the LAN web listener sharing st/env and
// deciding approvals through decider. It runs the Decision 0009 bind guard first: a
// non-loopback address refuses to bind without an active token, and
// --insecure-http is loud. decider is the caller's reactive engine, so an
// approval decided here releases the job into that process's queue. ntfy is
// the ntfy leg's seam (the daemon's judgment + test push); a caller that wants
// no push surface passes nil.
func startWebSurface(ctx context.Context, st *store.Store, env *secrets.Envelope, dir string,
	decider web.Decider, maint web.Maintenance, ntfy web.NtfyDriver, basePath, addr string, insecure bool, warn io.Writer) (*webSurface, error) {
	guardWarn, err := web.RequireTokenForBind(ctx, st, addr, insecure, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if guardWarn != "" {
		_, _ = fmt.Fprintln(warn, guardWarn)
	}

	var fingerprint string
	var tlsCert tls.Certificate
	if !insecure {
		cert, fp, cerr := web.EnsureCert(dir)
		if cerr != nil {
			return nil, cerr
		}
		tlsCert, fingerprint = cert, fp
	}

	srv, err := web.New(web.Options{
		Store:       st,
		Deriver:     env,
		Decider:     decider,
		Plans:       env,
		Sealer:      env,
		Maint:       maint,
		Ntfy:        ntfy,
		Insecure:    insecure,
		Fingerprint: fingerprint,
		Version:     version.String(),
		DataDir:     dir,
		BasePath:    basePath,
	})
	if err != nil {
		return nil, err
	}

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("web: listen: %w", err)
	}

	scheme := "https"
	if insecure {
		scheme = "http"
	}
	serveErr := make(chan error, 1)
	go func() {
		if insecure {
			serveErr <- httpSrv.Serve(ln)
			return
		}
		httpSrv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{tlsCert}, MinVersion: tls.VersionTLS12}
		serveErr <- httpSrv.ServeTLS(ln, "", "")
	}()

	return &webSurface{
		http:        httpSrv,
		srv:         srv,
		addr:        ln.Addr().String(),
		scheme:      scheme,
		fingerprint: fingerprint,
		serveErr:    serveErr,
	}, nil
}

// webEngine builds the reactive engine the web surface decides approvals
// through. It shares the store with a running daemon; its queue only transitions
// rows (release/withdraw), so it needs no workers and no live driver.
func webEngine(st *store.Store, env *secrets.Envelope) (*reactive.Engine, error) {
	q := queue.New(st, webNoRunDriver{}, queue.Options{})
	return reactive.New(reactive.Options{
		Store:    st,
		Queue:    q,
		Deriver:  env,
		Notifier: notify.New(nil),
	})
}

// webNoRunDriver is the driver the web process's queue never calls: this
// process releases gated jobs for the daemon to run, it does not run them
// itself. A call here is a bug, so it says so rather than doing nothing.
type webNoRunDriver struct{}

func (webNoRunDriver) Run(context.Context, executor.Spec, executor.EventSink) (executor.Result, error) {
	return executor.Result{}, errors.New("web: this process does not run jobs")
}
