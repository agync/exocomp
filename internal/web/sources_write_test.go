package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

func TestSourceAddPathReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/sources/add-path",
		url.Values{"name": {"tree"}, "kind": {"hostpath"}, "path": {"/srv/tree"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly add-path = %d, want 403", resp.StatusCode)
	}
}

func TestSourceAddPathCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/add-path",
		url.Values{"name": {"tree"}, "kind": {"hostpath"}, "path": {"/srv/tree"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin add-path = %d, want 403", resp.StatusCode)
	}
}

func TestSourceAddPathCreatesSource(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/add-path",
		url.Values{"name": {"tree"}, "kind": {"hostpath"}, "path": {"/srv/tree"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add-path = %d, want 303", resp.StatusCode)
	}
	src, err := h.store.SourceByName(context.Background(), "tree")
	if err != nil {
		t.Fatalf("source not created: %v", err)
	}
	if src.Kind != "hostpath" || src.URI != "/srv/tree" || !src.Enabled {
		t.Fatalf("unexpected source row: %+v", src)
	}
}

func TestSourceEnableDisable(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSource(ctx, store.Source{Name: "tree", Kind: "hostpath", URI: "/srv/tree", Enabled: true}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/sources/tree/disable", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("disable = %d, want 303", resp.StatusCode)
	}
	src, err := h.store.SourceByName(ctx, "tree")
	if err != nil {
		t.Fatalf("source by name: %v", err)
	}
	if src.Enabled {
		t.Fatal("source still enabled after disable")
	}

	resp = postForm(t, client, ts.URL+"/sources/tree/enable", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("enable = %d, want 303", resp.StatusCode)
	}
	src, err = h.store.SourceByName(ctx, "tree")
	if err != nil {
		t.Fatalf("source by name: %v", err)
	}
	if !src.Enabled {
		t.Fatal("source still disabled after enable")
	}
}

func TestSourceRm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSource(ctx, store.Source{Name: "tree", Kind: "hostpath", URI: "/srv/tree", Enabled: true}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/sources/tree/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm = %d, want 303", resp.StatusCode)
	}
	if _, err := h.store.SourceByName(ctx, "tree"); err == nil {
		t.Fatal("source still present after rm")
	}
}

func TestSourceAddGitNoSealer503(t *testing.T) {
	h := newHarness(t)
	// A server with no Sealer (a web-only process holding no envelope).
	noseal, err := New(Options{Store: h.store, Deriver: h.env, Version: "test", DataDir: h.dir})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	ts := httptest.NewServer(noseal.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/add-git",
		url.Values{"name": {"infra"}, "url": {"git@example.com:infra.git"}, "confirm_token": {op}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("add-git with nil Sealer = %d, want 503", resp.StatusCode)
	}
}

func TestSourceAddGitSealsDeployKey(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/add-git",
		url.Values{"name": {"infra"}, "url": {"git@example.com:infra.git"}, "confirm_token": {op}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add-git = %d, want 200: %s", resp.StatusCode, body)
	}
	if want := "ssh-ed25519"; !strings.Contains(body, want) {
		t.Fatalf("add-git result did not show the deploy public key once: %s", body)
	}
	src, err := h.store.SourceByName(context.Background(), "infra")
	if err != nil {
		t.Fatalf("source not created: %v", err)
	}
	if src.Kind != "git" || src.DeployKeySecret == "" {
		t.Fatalf("unexpected git source row: %+v", src)
	}
	if _, err := h.store.GetSecret(context.Background(), src.DeployKeySecret); err != nil {
		t.Fatalf("deploy key not sealed: %v", err)
	}
}
