package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestRunnerAddValidatesName(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/runners/add",
		url.Values{"name": {"Not Valid!"}, "base": {"registry.fedoraproject.org/fedora-minimal:44"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("an invalid runner name was accepted")
	}
	if _, err := h.store.RunnerByName(context.Background(), "Not Valid!"); err == nil {
		t.Fatal("a runner was created despite an invalid name")
	}
}

func TestRunnerAddRejectsBootstrapAndContainerfile(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/runners/add", url.Values{
		"name":          {"both"},
		"base":          {"registry.fedoraproject.org/fedora-minimal:44"},
		"bootstrap":     {"dnf install -y rsync"},
		"containerfile": {".exocomp/Containerfile"},
	}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("a runner with both a bootstrap and a containerfile was accepted")
	}
	if _, err := h.store.RunnerByName(context.Background(), "both"); err == nil {
		t.Fatal("a runner was created despite giving both a bootstrap and a containerfile")
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
