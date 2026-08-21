package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/store"
)

// fakeDeriver stands in for the secrets envelope: a deterministic subkey per
// purpose and id is all the backend asks of it.
type fakeDeriver struct{}

func (fakeDeriver) Derive(purpose, id string) []byte {
	return []byte("subkey/" + purpose + "/" + id + "/0123456789abcdef")
}

type backend struct {
	t      *testing.T
	store  *store.Store
	server *httptest.Server
}

func newBackend(t *testing.T) *backend {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	srv, err := api.New(api.Options{Store: st, Tokens: fakeDeriver{}})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	return &backend{t: t, store: st, server: hs}
}

func (b *backend) workspace(name string) store.Workspace {
	b.t.Helper()
	ws, err := b.store.CreateWorkspace(context.Background(), store.Workspace{
		Name:    name,
		TofuDir: "/srv/tofu/" + name,
	})
	if err != nil {
		b.t.Fatalf("create workspace: %v", err)
	}
	return ws
}

// do issues a state request. An empty token sends no Authorization header.
func (b *backend) do(method, workspace, query, token, body string) *http.Response {
	b.t.Helper()

	url := b.server.URL + api.StatePrefix + workspace
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		b.t.Fatalf("build %s request: %v", method, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := b.server.Client().Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, url, err)
	}
	b.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (b *backend) token(workspace string) string {
	return api.WorkspaceToken(fakeDeriver{}, workspace)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	blob, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(blob)
}

func lockBody(id, who string) string {
	return fmt.Sprintf(`{"ID":%q,"Operation":"OperationTypeApply","Who":%q,"Version":"1.11.5"}`, id, who)
}

func TestHealthzNeedsNoToken(t *testing.T) {
	b := newBackend(t)

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, b.server.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := b.server.Client().Do(req)
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := strings.TrimSpace(readBody(t, resp)); got != "ok" {
		t.Errorf("body = %q, want ok", got)
	}
}

func TestStateRejectsBadTokens(t *testing.T) {
	b := newBackend(t)
	b.workspace("edge")
	b.workspace("core")

	cases := []struct {
		name  string
		token string
	}{
		{"no token", ""},
		{"wrong token", "not-the-token"},
		{"another workspace's token", api.WorkspaceToken(fakeDeriver{}, "core")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := b.do(http.MethodGet, "edge", "", tc.token, "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}

	// The right token still works, so the rejections above are about the
	// token and not about the route.
	if resp := b.do(http.MethodGet, "edge", "", b.token("edge"), ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("authorized GET without state = %d, want 404", resp.StatusCode)
	}
}

func TestStateUnknownWorkspaceIsNotFound(t *testing.T) {
	b := newBackend(t)
	resp := b.do(http.MethodGet, "absent", "", b.token("absent"), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestStateRoundTrip(t *testing.T) {
	b := newBackend(t)
	ws := b.workspace("edge")
	token := b.token("edge")

	if resp := b.do("LOCK", "edge", "", token, lockBody("lock-1", "operator@host")); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock status = %d, want 200", resp.StatusCode)
	}

	const body = `{"serial":1,"lineage":"1e5f-abcd","encrypted_data":"opaque"}`
	resp := b.do(http.MethodPost, "edge", "ID=lock-1", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post status = %d (%s), want 200", resp.StatusCode, readBody(t, resp))
	}

	got := b.do(http.MethodGet, "edge", "", token, "")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", got.StatusCode)
	}
	if payload := readBody(t, got); payload != body {
		t.Errorf("state = %s, want it returned verbatim", payload)
	}

	stored, err := b.store.LatestTFState(context.Background(), ws.ID)
	if err != nil {
		t.Fatalf("latest tfstate: %v", err)
	}
	if stored.Serial != 1 || stored.Lineage != "1e5f-abcd" {
		t.Errorf("stored = serial %d lineage %q, want 1 and 1e5f-abcd", stored.Serial, stored.Lineage)
	}

	if resp := b.do("UNLOCK", "edge", "", token, lockBody("lock-1", "operator@host")); resp.StatusCode != http.StatusOK {
		t.Errorf("unlock status = %d, want 200", resp.StatusCode)
	}
}

func TestLockConflictReportsHolder(t *testing.T) {
	b := newBackend(t)
	b.workspace("edge")
	token := b.token("edge")

	if resp := b.do("LOCK", "edge", "", token, lockBody("lock-1", "first@host")); resp.StatusCode != http.StatusOK {
		t.Fatalf("first lock = %d, want 200", resp.StatusCode)
	}

	resp := b.do("LOCK", "edge", "", token, lockBody("lock-2", "second@host"))
	if resp.StatusCode != http.StatusLocked {
		t.Fatalf("second lock = %d, want 423", resp.StatusCode)
	}

	var holder struct {
		ID        string `json:"ID"`
		Who       string `json:"Who"`
		Operation string `json:"Operation"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &holder); err != nil {
		t.Fatalf("decode holder: %v", err)
	}
	if holder.ID != "lock-1" || holder.Who != "first@host" {
		t.Errorf("holder = %+v, want the first lock", holder)
	}
	if holder.Operation != "OperationTypeApply" {
		t.Errorf("holder operation = %q", holder.Operation)
	}
}

func TestPostRequiresTheLockHolder(t *testing.T) {
	b := newBackend(t)
	ws := b.workspace("edge")
	token := b.token("edge")

	if resp := b.do("LOCK", "edge", "", token, lockBody("lock-1", "first@host")); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock = %d, want 200", resp.StatusCode)
	}

	for _, query := range []string{"ID=lock-2", ""} {
		resp := b.do(http.MethodPost, "edge", query, token, `{"serial":1}`)
		if resp.StatusCode != http.StatusLocked {
			t.Errorf("post with %q = %d, want 423", query, resp.StatusCode)
		}
	}

	if _, err := b.store.LatestTFState(context.Background(), ws.ID); err == nil {
		t.Error("a refused push still stored a version")
	}
}

func TestUnlockRequiresTheLockHolder(t *testing.T) {
	b := newBackend(t)
	b.workspace("edge")
	token := b.token("edge")

	if resp := b.do("LOCK", "edge", "", token, lockBody("lock-1", "first@host")); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock = %d, want 200", resp.StatusCode)
	}
	if resp := b.do("UNLOCK", "edge", "", token, lockBody("lock-2", "second@host")); resp.StatusCode != http.StatusLocked {
		t.Errorf("unlock by a non-holder = %d, want 423", resp.StatusCode)
	}
	if resp := b.do("UNLOCK", "edge", "", token, lockBody("lock-1", "first@host")); resp.StatusCode != http.StatusOK {
		t.Errorf("unlock by the holder = %d, want 200", resp.StatusCode)
	}
}

func TestStateRejectsMalformedLockAndOtherMethods(t *testing.T) {
	b := newBackend(t)
	b.workspace("edge")
	token := b.token("edge")

	if resp := b.do("LOCK", "edge", "", token, "not json"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed lock = %d, want 400", resp.StatusCode)
	}
	if resp := b.do("LOCK", "edge", "", token, `{"Who":"nobody"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("lock without an ID = %d, want 400", resp.StatusCode)
	}
	if resp := b.do(http.MethodPost, "edge", "", token, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty push = %d, want 400", resp.StatusCode)
	}
	if resp := b.do(http.MethodDelete, "edge", "", token, ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("delete = %d, want 405", resp.StatusCode)
	}
}

func TestStartReportsItsAddress(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	srv, err := api.New(api.Options{Store: st, Tokens: fakeDeriver{}, Port: 0})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	addr := srv.Addr()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("addr = %q, want a loopback address", addr)
	}
	if port := api.PortOf(addr); port == 0 {
		t.Errorf("port of %q = 0, want the ephemeral port it bound", addr)
	}
	if !api.Reachable(context.Background(), addr) {
		t.Error("server is not reachable at its own address")
	}
	if want := "http://" + addr + api.StatePrefix + "edge"; api.StateURL(addr, "edge") != want {
		t.Errorf("state url = %q, want %q", api.StateURL(addr, "edge"), want)
	}
}

func TestAddressFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), api.AddressFile)

	if addr, err := api.ReadAddress(path); err != nil || addr != "" {
		t.Fatalf("read missing address file = %q, %v", addr, err)
	}

	// An address nothing serves any more: bound to learn a free port, then
	// released. That is exactly the stale entry a finished run leaves behind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stale := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	if err := api.WriteAddress(path, stale); err != nil {
		t.Fatalf("write address: %v", err)
	}
	addr, err := api.ReadAddress(path)
	if err != nil {
		t.Fatalf("read address: %v", err)
	}
	if addr != stale || api.PortOf(addr) == 0 {
		t.Errorf("address = %q, port = %d, want %q", addr, api.PortOf(addr), stale)
	}
	if api.Reachable(context.Background(), addr) {
		t.Error("a recorded address with nothing behind it reported reachable")
	}
}
