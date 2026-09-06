package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/deps"
	"exocomp.dev/exocomp/internal/store"
)

func TestSourceAddPathReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"tree"}, "location": {"/srv/tree"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly add = %d, want 403", resp.StatusCode)
	}
}

func TestSourceAddPathCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"tree"}, "location": {"/srv/tree"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin add = %d, want 403", resp.StatusCode)
	}
}

func TestSourceAddPathCreatesSource(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"tree"}, "location": {"/srv/tree"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add = %d, want 303", resp.StatusCode)
	}
	src, err := h.store.SourceByName(context.Background(), "tree")
	if err != nil {
		t.Fatalf("source not created: %v", err)
	}
	// A path source defaults to read-only (Writable false).
	if src.Kind != store.SourcePath || src.URI != "/srv/tree" || src.Writable || !src.Enabled {
		t.Fatalf("unexpected source row: %+v", src)
	}

	// The writable checkbox opts a path source into a read+write tree.
	resp = postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"rwtree"}, "location": {"/srv/rw"}, "writable": {"on"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("writable add = %d, want 303", resp.StatusCode)
	}
	rw, err := h.store.SourceByName(context.Background(), "rwtree")
	if err != nil {
		t.Fatalf("writable source not created: %v", err)
	}
	if rw.Kind != store.SourcePath || !rw.Writable {
		t.Fatalf("unexpected writable source row: %+v", rw)
	}
}

func TestSourceEnableDisable(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSource(ctx, store.Source{Name: "tree", Kind: store.SourcePath, URI: "/srv/tree", Enabled: true}); err != nil {
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

	if _, err := h.store.CreateSource(ctx, store.Source{Name: "tree", Kind: store.SourcePath, URI: "/srv/tree", Enabled: true}); err != nil {
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

func TestSourceRmPurgesCaches(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSource(ctx, store.Source{Name: "tree", Kind: store.SourcePath, URI: "/srv/tree", Enabled: true}); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	python, collections := plantWebCaches(t, h.dir, "/srv/tree")

	resp := postForm(t, client, ts.URL+"/sources/tree/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm = %d, want 303", resp.StatusCode)
	}
	if _, err := os.Stat(python); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache/python kept after web rm of a sole source: %v", err)
	}
	if _, err := os.Stat(collections); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache/collections kept after web rm of a sole source: %v", err)
	}
}

func TestSourceRmSharedRootKeepsCaches(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	for _, name := range []string{"a", "b"} {
		if _, err := h.store.CreateSource(ctx, store.Source{Name: name, Kind: store.SourcePath, URI: "/srv/shared", Enabled: true}); err != nil {
			t.Fatalf("seed source %s: %v", name, err)
		}
	}
	python, collections := plantWebCaches(t, h.dir, "/srv/shared")

	resp := postForm(t, client, ts.URL+"/sources/a/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm = %d, want 303", resp.StatusCode)
	}
	if _, err := os.Stat(python); err != nil {
		t.Fatalf("cache/python purged despite a sibling sharing the root: %v", err)
	}
	if _, err := os.Stat(collections); err != nil {
		t.Fatalf("cache/collections purged despite a sibling sharing the root: %v", err)
	}
}

// TestSourceRmDisabledSiblingKeepsCaches proves the guard counts a disabled
// source as a reference: the sibling still owns the cache when re-enabled.
func TestSourceRmDisabledSiblingKeepsCaches(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSource(ctx, store.Source{Name: "a", Kind: store.SourcePath, URI: "/srv/shared", Enabled: true}); err != nil {
		t.Fatalf("seed source a: %v", err)
	}
	if _, err := h.store.CreateSource(ctx, store.Source{Name: "b", Kind: store.SourcePath, URI: "/srv/shared", Enabled: true}); err != nil {
		t.Fatalf("seed source b: %v", err)
	}
	b, err := h.store.SourceByName(ctx, "b")
	if err != nil {
		t.Fatalf("source b by name: %v", err)
	}
	if err := h.store.SetSourceEnabled(ctx, b.ID, false); err != nil {
		t.Fatalf("disable b: %v", err)
	}
	python, collections := plantWebCaches(t, h.dir, "/srv/shared")

	resp := postForm(t, client, ts.URL+"/sources/a/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm = %d, want 303", resp.StatusCode)
	}
	if _, err := os.Stat(python); err != nil {
		t.Fatalf("cache/python purged despite a disabled sibling owning the root: %v", err)
	}
	if _, err := os.Stat(collections); err != nil {
		t.Fatalf("cache/collections purged despite a disabled sibling owning the root: %v", err)
	}
}

// plantWebCaches creates the dependency-cache planes a synced source leaves
// under the harness data dir for the given root, returning both plane roots.
func plantWebCaches(t *testing.T, dir, root string) (python, collections string) {
	t.Helper()
	key := deps.SourceKey(root)
	python = filepath.Join(dir, "cache", "python", key)
	collections = filepath.Join(dir, "cache", "collections", key)
	for _, d := range []string{
		filepath.Join(python, "venv"),
		filepath.Join(collections, "ansible_collections"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("plant cache: %v", err)
		}
	}
	return python, collections
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

	resp := postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"infra"}, "location": {"git@example.com:infra.git"}, "confirm_token": {op}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("add git-looking location with nil Sealer = %d, want 503", resp.StatusCode)
	}
}

func TestSourceAddGitSealsDeployKey(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"infra"}, "location": {"git@example.com:infra.git"}, "confirm_token": {op}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add git-looking location = %d, want 200: %s", resp.StatusCode, body)
	}
	if want := "ssh-ed25519"; !strings.Contains(body, want) {
		t.Fatalf("add result did not show the deploy public key once: %s", body)
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

// TestSourceAddRoutesOnLocation proves the single POST /sources/add endpoint
// dispatches on source.LooksLikeGitURL(location): a plain filesystem path
// lands on addPathSource (no reauth needed, no deploy key), and a git-looking
// location lands on addGitSource (requires reauth, seals a deploy key) —
// through the same route, without a kind field telling it which.
func TestSourceAddRoutesOnLocation(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	// A plain path routes to addPathSource: no confirm_token needed, and the
	// row lands as a path source, not git.
	resp := postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"pathy"}, "location": {"/srv/pathy"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("path-looking add = %d, want 303", resp.StatusCode)
	}
	pathSrc, err := h.store.SourceByName(context.Background(), "pathy")
	if err != nil {
		t.Fatalf("path source not created: %v", err)
	}
	if pathSrc.Kind != store.SourcePath {
		t.Fatalf("path-looking location routed to kind %q, want %q", pathSrc.Kind, store.SourcePath)
	}

	// A git-looking location routes to addGitSource: it is gated on reauth
	// (missing/wrong confirm_token bounces to the reauth notice) and, once
	// confirmed, lands as a git source with a sealed deploy key.
	resp = postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"gitty"}, "location": {"git@example.com:gitty.git"}, "confirm_token": {"wrong"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "did not match this session") {
		t.Fatalf("git-looking add with wrong confirm_token = %d, want the reauth notice: %s", resp.StatusCode, body)
	}
	if _, err := h.store.SourceByName(context.Background(), "gitty"); err == nil {
		t.Fatal("git source created despite failed reauth")
	}

	resp = postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"gitty"}, "location": {"git@example.com:gitty.git"}, "confirm_token": {op}}, nil)
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("git-looking add = %d, want 200: %s", resp.StatusCode, body)
	}
	gitSrc, err := h.store.SourceByName(context.Background(), "gitty")
	if err != nil {
		t.Fatalf("git source not created: %v", err)
	}
	if gitSrc.Kind != store.SourceGit || gitSrc.DeployKeySecret == "" {
		t.Fatalf("git-looking location routed to unexpected row: %+v", gitSrc)
	}
}
