package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
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

func TestSourceAddBadNameIs422(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	// A name outside the [A-Za-z0-9._-] rule (a space) is the operator's
	// input error: the dialog re-renders at 422 with the ▲ field refusal and
	// the typed value kept — the netgroup pattern, never a 500 "internal
	// error".
	resp := postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"Home Lab"}, "location": {"/srv/tree"}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad-name add = %d, want 422 (%s)", resp.StatusCode, body)
	}
	for _, want := range []string{
		`id="sources-add"`, // the dialog re-renders open
		`value="Home Lab"`, // the typed value survives
		`Use letters, digits, dots, hyphens, underscores.`, // the field's human copy
		`aria-invalid="true"`,                              // on the invalid control
	} {
		if !strings.Contains(body, want) {
			t.Errorf("422 source-add missing %q", want)
		}
	}
	if _, err := h.store.SourceByName(context.Background(), "Home Lab"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bad-name source must not be created (err=%v)", err)
	}
}

func TestSourceAddEmptyLocationIs422(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/add",
		url.Values{"name": {"homelab"}, "location": {""}}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty-location add = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `A path or git URL is required.`) {
		t.Errorf("422 source-add missing the location refusal: %s", body)
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
	body := readBody(t, resp)
	// No sealer is a submit-time refusal, not a bare 503: the dialog
	// re-renders at 422 with the status line naming it (values kept).
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("add git-looking location with nil Sealer = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Sealing is not available on this surface") {
		t.Errorf("no-sealer 422 missing the status-line refusal: %s", body)
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

// TestSourcesAppbarMetaIsSourceCount: the app bar's meta slot carries the
// page's own fact — the registered source count — with the singular fixed.
func TestSourcesAppbarMetaIsSourceCount(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/sources")
	if code != http.StatusOK {
		t.Fatalf("/sources status = %d", code)
	}
	if !strings.Contains(page, `<span class="appbar-meta">0 sources</span>`) {
		t.Errorf("sources app bar missing the zero count meta:\n%s", page)
	}

	if _, err := h.store.CreateSource(context.Background(), store.Source{
		Name: "one", Kind: store.SourcePath, URI: "/srv/one", Enabled: true,
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	_, page = getBody(t, client, ts.URL+"/sources")
	if !strings.Contains(page, `<span class="appbar-meta">1 source</span>`) {
		t.Errorf("sources app bar missing the singular count meta:\n%s", page)
	}
}

// TestSourceRmRequiresConfirmation: a confirmed-less remove re-renders the
// source detail page at 422 with the form-status refusal — never a bare error
// page — and nothing is removed.
func TestSourceRmRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSource(ctx, store.Source{Name: "tree", Kind: store.SourcePath, URI: "/srv/tree", Enabled: true}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/sources/tree/rm", url.Values{}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("rm refusal status = %d, want 422 (%s)", resp.StatusCode, body)
	}
	for _, want := range []string{
		`form-status is-error`,
		"Nothing was removed — confirm the removal in the dialog.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rm refusal missing %q", want)
		}
	}
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("rm refusal flashed")
		}
	}
	if _, err := h.store.SourceByName(ctx, "tree"); err != nil {
		t.Fatal("refused rm removed the source")
	}
}

func TestSourceSyncReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/sources/tree/sync", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly sync = %d, want 403", resp.StatusCode)
	}
}

func TestSourceSyncCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/tree/sync",
		url.Values{},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin sync = %d, want 403", resp.StatusCode)
	}
}

func TestSourceSyncNotFound(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/sources/nonexistent/sync", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nonexistent source sync = %d, want 404", resp.StatusCode)
	}
}

func TestSourceSyncPathRefused(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSource(ctx, store.Source{Name: "tree", Kind: store.SourcePath, URI: "/srv/tree", Enabled: true}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/sources/tree/sync", url.Values{}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("path sync refusal status = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "only git sources sync") {
		t.Errorf("path sync refusal missing expected error: %s", body)
	}
}

// gitEnv is a clean environment for the throwaway repos: it deliberately does
// not inherit the parent's. This repo's own pre-commit hook runs these tests,
// and a hook's environment carries GIT_DIR/GIT_INDEX_FILE as well as (on git
// 2.54+) GIT_CONFIG_COUNT/KEY/VALUE seeding core.hooksPath at the outer repo's
// hooks. Inheriting either makes `git -C <tmp>` act on the outer repo, or shell
// out to a hook that finds no .pre-commit-config.yaml in the temp tree.
func gitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
}

func testGitRepo(t *testing.T) (repoPath, headSHA string) {
	t.Helper()
	repoPath = t.TempDir()
	home := t.TempDir()
	cmd := exec.Command("git", "-C", repoPath, "init", "-b", "main")
	cmd.Env = gitEnv(home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "site.yml"), []byte("- hosts: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "site.yml"},
		{"-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial"},
	} {
		cmd = exec.Command("git", append([]string{"-C", repoPath}, args...)...)
		cmd.Env = gitEnv(home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	cmd = exec.Command("git", "-C", repoPath, "rev-parse", "HEAD")
	cmd.Env = gitEnv(home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v: %s", err, out)
	}
	return repoPath, strings.TrimSpace(string(out))
}

func TestSourceSyncGitSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real git")
	}
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	repoPath, wantSHA := testGitRepo(t)
	if _, err := h.store.CreateSource(ctx, store.Source{
		Name: "git-infra", Kind: store.SourceGit, URI: repoPath, Ref: "refs/heads/main", Enabled: true,
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/sources/git-infra/sync", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("git sync status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/sources/git-infra" {
		t.Errorf("sync Location = %q, want /sources/git-infra", loc)
	}

	var foundFlash bool
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && strings.Contains(c.Value, "synced git-infra") {
			foundFlash = true
		}
	}
	if !foundFlash {
		t.Error("sync did not set flash cookie")
	}

	// Verify store updated with SHA
	src, err := h.store.SourceByName(ctx, "git-infra")
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if src.LastSyncSHA != wantSHA {
		t.Errorf("source LastSyncSHA = %q, want %q", src.LastSyncSHA, wantSHA)
	}
}
