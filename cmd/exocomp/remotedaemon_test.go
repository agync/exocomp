package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/web"
)

// testDaemon is a daemon's API surface under test: a real store and envelope in
// their own data dir, the LAN listener on an ephemeral loopback port, and a
// queue worker whose driver records each job instead of running it. The CLI
// under test keeps a separate local data dir, so a test can tell which store a
// command wrote to.
type testDaemon struct {
	dir      string
	st       *store.Store
	env      *secrets.Envelope
	endpoint string
	fp       string

	mu  sync.Mutex
	ran []executor.Spec
	// exitCode is what every job the worker runs exits with.
	exitCode int
}

// recordingDriver settles every job with the daemon's exitCode (0 unless a test
// sets it) and one line of command-module output, and keeps the spec it was
// handed.
type recordingDriver struct{ d *testDaemon }

func (r recordingDriver) Run(_ context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	r.d.mu.Lock()
	r.d.ran = append(r.d.ran, spec)
	code := r.d.exitCode
	r.d.mu.Unlock()
	sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: "stdout", Msg: "node0 | CHANGED | rc=0 >>"})
	sink.Emit(executor.Event{Ts: time.Now().UTC(), Level: "stdout", Msg: "hello"})
	return executor.Result{ExitCode: code}, nil
}

// startTestDaemon starts the surface and its worker, and stops both when the
// test ends. It points the CLI's own data dir and hosts.toml at fresh temp dirs.
func startTestDaemon(t *testing.T) *testDaemon {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	withConfigHome(t)

	d := &testDaemon{dir: filepath.Join(t.TempDir(), "daemon")}
	env, _, err := secrets.Init(d.dir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	d.env = env
	t.Cleanup(func() { _ = env.Close() })
	d.st, err = openStoreAt(d.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	ws, err := startWebSurface(ctx, d.st, env, d.dir, nil, nil, nil, "", "127.0.0.1:0", false, nil, io.Discard)
	if err != nil {
		cancel()
		t.Fatalf("start web surface: %v", err)
	}
	d.endpoint, d.fp = "https://"+ws.addr, ws.fingerprint

	q := queue.New(d.st, recordingDriver{d}, queue.Options{Workers: 1})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = q.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = ws.Shutdown(shutdown)
	})
	return d
}

// pin mints a token of the given scope on the daemon and stores it in the
// CLI's hosts.toml under alias, as `exocomp login` would.
func (d *testDaemon) pin(t *testing.T, alias, scope string, isDefault bool) {
	t.Helper()
	secret, _, err := web.MintToken(context.Background(), d.st, alias+"-"+scope, scope, "", 0)
	if err != nil {
		t.Fatalf("mint %s token: %v", scope, err)
	}
	cfg, err := loadHostsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hosts == nil {
		cfg.Hosts = map[string]hostEntry{}
	}
	cfg.Hosts[alias] = hostEntry{Endpoint: d.endpoint, Fingerprint: d.fp, Token: secret}
	if isDefault {
		cfg.Default = alias
	}
	if err := saveHostsConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// specs returns what the worker has been handed so far.
func (d *testDaemon) specs() []executor.Spec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]executor.Spec(nil), d.ran...)
}

// localDataDir is the CLI's own data dir, which a remote command must leave
// untouched.
func localDataDir(t *testing.T) string {
	t.Helper()
	dir, err := dataDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// assertNoLocalStore fails when the CLI created a local data dir.
func assertNoLocalStore(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(localDataDir(t)); !os.IsNotExist(err) {
		t.Fatalf("the CLI's local data dir was touched: %v", err)
	}
}
