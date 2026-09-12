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

func TestRunnerWriteReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/runners/add",
		url.Values{"name": {"fedora-ee"}, "base": {"registry.fedoraproject.org/fedora-minimal:44"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly runner add = %d, want 403", resp.StatusCode)
	}
}

func TestRunnerWriteCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/runners/add",
		url.Values{"name": {"fedora-ee"}, "base": {"registry.fedoraproject.org/fedora-minimal:44"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin runner add = %d, want 403", resp.StatusCode)
	}
}

func TestRunnerAddCreates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	resp := postForm(t, client, ts.URL+"/runners/add", url.Values{
		"name":         {"fedora-ee"},
		"mode":         {"cached"},
		"base":         {"registry.fedoraproject.org/fedora-minimal:44"},
		"packages":     {"sshpass, rsync"},
		"ansible_core": {"2.18.1"},
		"memory_max":   {"2G"},
		"cpu_quota":    {"200%"},
	}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("runner add = %d, want 303", resp.StatusCode)
	}

	rows, err := h.store.ListRunners(ctx)
	if err != nil {
		t.Fatalf("ListRunners: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Name == "fedora-ee" {
			found = true
			if r.Mode != store.RunnerModeCached {
				t.Fatalf("mode = %q, want cached", r.Mode)
			}
			if r.Base != "registry.fedoraproject.org/fedora-minimal:44" {
				t.Fatalf("base = %q", r.Base)
			}
			if len(r.Packages) != 2 || r.Packages[0] != "rsync" || r.Packages[1] != "sshpass" {
				t.Fatalf("packages = %v, want canonical [rsync sshpass]", r.Packages)
			}
		}
	}
	if !found {
		t.Fatal("runner was not created")
	}
}

// TestRunnerFormAtRest renders the runners-add dialog for an operator and
// asserts the forms-round anatomy: the reusable field pattern with
// always-present meta lines (help at rest), the mode select and the bootstrap
// textarea's own meta lines, aria wiring tying each control to its meta, a
// form-status line, and novalidate so the daemon stays authoritative.
func TestRunnerFormAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/runners")
	if code != http.StatusOK {
		t.Fatalf("/runners = %d, want 200", code)
	}
	for _, want := range []string{
		`id="runners-add"`,
		`action="/runners/add"`,
		`id="runner-add-form" novalidate`,
		`aria-describedby="rn-name-meta"`,
		`aria-describedby="rn-mode-meta"`,
		`aria-describedby="rn-base-meta"`,
		`aria-describedby="rn-bootstrap-meta"`,
		`aria-describedby="rn-containerfile-meta"`,
		`id="rn-name-meta"`,
		`id="rn-mode-meta"`,
		`id="rn-base-meta"`,
		`id="rn-bootstrap-meta"`,
		`id="rn-containerfile-meta"`,
		"Mode is cached or onthefly.",
		"A base image reference, like registry.fedoraproject.org/fedora-minimal:44.",
		"A Containerfile path in the source (Model B).",
		`role="status" aria-live="polite"`,
		"Add runner",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("runner form missing %q", want)
		}
	}
	// At rest no control carries a refusal and the metas are plain help text.
	if !strings.Contains(page, `placeholder="fedora-ee" aria-describedby="rn-name-meta">`) {
		t.Error("at-rest name control carries an aria-invalid refusal")
	}
	if !strings.Contains(page, `id="rn-name-meta" class="meta">A lowercase`) {
		t.Error("at-rest name meta is not plain help text")
	}
}

// TestRunnerFormRefusalsRender asserts the 422 re-render fills every meta line
// at once (name, mode, base, containerfile), keeps the typed values, and never
// flashes: a refusal is the form's own surface, flash is success-only.
func TestRunnerFormRefusalsRender(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/runners/add", url.Values{
		"name":          {"Not Valid!"},
		"mode":          {"nonsense"},
		"base":          {""},
		"bootstrap":     {"dnf install -y rsync"},
		"containerfile": {".exocomp/Containerfile"},
	}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("refusal status = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{
		`aria-invalid="true"`,
		"Names start with a lowercase letter or digit",
		"Mode is cached or onthefly.",
		"Give the base image a name, like registry.fedoraproject.org/fedora-minimal:44.",
		"Give a bootstrap script or a Containerfile path — not both.",
		`value="Not Valid!"`,
		`exoModalOpen("runners-add")`,
		`autofocus`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("422 re-render missing %q", want)
		}
	}
	// A refusal never flashes: the render consumes the flash cookie (an empty
	// clearing Set-Cookie), so no flash MESSAGE may ride this response.
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("runner refusal carried a flash message")
		}
	}
}

// TestRunnerAddValidatesName: a name the guard refuses re-renders the form at
// 422 with the name field's human sentence — never the validator's regex —
// and no row lands.
func TestRunnerAddValidatesName(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/runners/add",
		url.Values{"name": {"Not Valid!"}, "base": {"registry.fedoraproject.org/fedora-minimal:44"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("name refusal status = %d, want 422", resp.StatusCode)
	}
	if strings.Contains(body, "must start with a lowercase letter") {
		t.Error("name refusal leaks the validator's constraint")
	}
	if !strings.Contains(body, "Names start with a lowercase letter or digit") {
		t.Errorf("name refusal missing the human sentence")
	}
	if _, err := h.store.RunnerByName(context.Background(), "Not Valid!"); err == nil {
		t.Fatal("a runner was created despite an invalid name")
	}
}

// TestRunnerAddRejectsBootstrapAndContainerfile: giving both build models
// re-renders the form at 422 with the containerfile field's sentence, and no
// row lands.
func TestRunnerAddRejectsBootstrapAndContainerfile(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/runners/add", url.Values{
		"name":          {"both"},
		"base":          {"registry.fedoraproject.org/fedora-minimal:44"},
		"bootstrap":     {"dnf install -y rsync"},
		"containerfile": {".exocomp/Containerfile"},
	}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("both-models status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Give a bootstrap script or a Containerfile path — not both.") {
		t.Errorf("both-models refusal missing the containerfile-field sentence")
	}
	if _, err := h.store.RunnerByName(context.Background(), "both"); err == nil {
		t.Fatal("a runner was created despite giving both a bootstrap and a containerfile")
	}
}

// TestRunnerRmRefusalRendersOnSurface: removing a runner that is already gone
// re-renders the surface at 404 with the refusal shown on it — never a flash
// (flash is success-only) — and nothing crashes.
func TestRunnerRmRefusalRendersOnSurface(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/runners/nope/rm", url.Values{"confirmed": {"1"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rm refusal status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body, "No runner named nope exists to remove.") {
		t.Errorf("rm refusal missing the on-surface sentence")
	}
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("rm refusal carried a flash message")
		}
	}
}

func TestRunnerRm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateRunner(ctx, store.Runner{
		Name: "dev-shell", Mode: store.RunnerModeOnTheFly,
		Base:          "registry.fedoraproject.org/fedora-minimal:44",
		Containerfile: ".exocomp/Containerfile",
	}); err != nil {
		t.Fatalf("CreateRunner: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/runners/dev-shell/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm = %d, want 303", resp.StatusCode)
	}
	if _, err := h.store.RunnerByName(ctx, "dev-shell"); err == nil {
		t.Fatal("runner still present after rm")
	}
}

func TestRunnerRmReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	if _, err := h.store.CreateRunner(ctx, store.Runner{
		Name: "fedora-ee", Mode: store.RunnerModeCached,
		Base: "registry.fedoraproject.org/fedora-minimal:44",
	}); err != nil {
		t.Fatalf("CreateRunner: %v", err)
	}
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/runners/fedora-ee/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly runner rm = %d, want 403", resp.StatusCode)
	}
	if _, err := h.store.RunnerByName(ctx, "fedora-ee"); err != nil {
		t.Fatal("runner removed despite readonly rm attempt")
	}
}
