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
}

func newWebCmd() *cobra.Command {
	opts := webOptions{addr: "0.0.0.0:8443"}

	cmd := &cobra.Command{
		Use:   "web",
		Short: "Serve the token-gated LAN web UI (ADR 0009)",
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

	// The bind guard is the ADR 0009 gate: a LAN address needs a token, and
	// --insecure-http is loud.
	guardWarn, err := web.RequireTokenForBind(ctx, st, opts.addr, opts.insecure, time.Now().UTC())
	if err != nil {
		return err
	}
	if guardWarn != "" {
		_, _ = fmt.Fprintln(warn, guardWarn)
	}

	engine, err := webEngine(st, env)
	if err != nil {
		return err
	}

	var fingerprint string
	var tlsCert tls.Certificate
	if !opts.insecure {
		cert, fp, cerr := web.EnsureCert(dir)
		if cerr != nil {
			return cerr
		}
		tlsCert, fingerprint = cert, fp
	}

	srv, err := web.New(web.Options{
		Store:       st,
		Deriver:     env,
		Decider:     engine,
		Plans:       env,
		Insecure:    opts.insecure,
		Fingerprint: fingerprint,
		Version:     version.String(),
		DataDir:     dir,
	})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", opts.addr)
	if err != nil {
		return fmt.Errorf("web: listen: %w", err)
	}

	scheme := "https"
	if opts.insecure {
		scheme = "http"
	}
	_, _ = fmt.Fprintf(out, "web UI listening on %s://%s\n", scheme, ln.Addr())
	if fingerprint != "" {
		_, _ = fmt.Fprintf(out, "certificate SHA-256: %s\n", fingerprint)
	}

	serveErr := make(chan error, 1)
	go func() {
		if opts.insecure {
			serveErr <- httpSrv.Serve(ln)
			return
		}
		httpSrv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{tlsCert}, MinVersion: tls.VersionTLS12}
		serveErr <- httpSrv.ServeTLS(ln, "", "")
	}()

	select {
	case <-sigCtx.Done():
		_, _ = fmt.Fprintln(out, "shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webShutdownGrace)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case e := <-serveErr:
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	}
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
