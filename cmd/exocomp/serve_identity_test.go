package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/web"
)

// TestServeRefusesABadIdentityAllowlist: serve checks --allow-ssh-identity
// before it starts anything, so an entry that would widen the grant stops it.
// The context is already cancelled, so a serve that skipped the check returns
// some other error instead of running.
func TestServeRefusesABadIdentityAllowlist(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, bad := range []string{"keys/id", t.TempDir(), filepath.Join(t.TempDir(), "absent")} {
		opts := defaultServeOptions()
		opts.identityAllow = []string{bad}
		err := runServe(ctx, io.Discard, io.Discard, opts)
		if err == nil || !strings.Contains(err.Error(), "--allow-ssh-identity") {
			t.Errorf("serve with --allow-ssh-identity %s = %v, want it refused", bad, err)
		}
	}
}

// TestWebSurfaceCarriesTheIdentityAllowlist: the list serve validated reaches
// the API face, so a remote run naming a listed key lands it on the job row.
func TestWebSurfaceCarriesTheIdentityAllowlist(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	dir := initForSource(t)
	ctx := context.Background()
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(key, []byte("not-a-real-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	defer func() { _ = env.Close() }()
	engine, err := webEngine(st, env)
	if err != nil {
		t.Fatalf("web engine: %v", err)
	}
	token, _, err := web.MintToken(ctx, st, "laptop", "operator", "", 0)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	surface, err := startWebSurface(ctx, st, env, dir, engine, nil, nil, "", "127.0.0.1:0", true,
		[]string{key}, io.Discard)
	if err != nil {
		t.Fatalf("start web surface: %v", err)
	}
	defer func() {
		shCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_ = surface.Shutdown(shCtx)
	}()

	_, port, err := net.SplitHostPort(surface.addr)
	if err != nil {
		t.Fatalf("split surface addr %q: %v", surface.addr, err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+port+"/api/v1/playbooks/run",
		strings.NewReader(`{"playbook":"/srv/site.yml","ssh_identity_files":["`+key+`"]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST playbooks/run: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	jobs, err := st.ListJobs(ctx, store.JobFilter{Runbook: "playbook"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || !slices.Equal(jobs[0].Args.SSHIdentityFiles, []string{key}) {
		t.Fatalf("jobs = %+v, want one carrying %s", jobs, key)
	}
}
