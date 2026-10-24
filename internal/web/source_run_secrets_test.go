package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// The source-scoped default run-secret binding (Decision 0024) has a read face
// (the identity card's Run-secrets line, the rules page's secrets guard) and an
// operator write face (POST /sources/{name}/run-secrets). These tests pin both.

// seedRunSecretBinding registers one pair under binding so the web verb has a
// real binding to accept.
func seedRunSecretBinding(t *testing.T, h *harness, binding string) {
	t.Helper()
	if _, err := h.store.AddRunSecret(context.Background(), store.RunSecret{
		Binding: binding, EnvName: "VAULT_PASSWORD", SecretName: "vault-pass",
	}); err != nil {
		t.Fatalf("AddRunSecret %s: %v", binding, err)
	}
}

func defaultRunSecrets(t *testing.T, h *harness, name string) string {
	t.Helper()
	src, err := h.store.SourceByName(context.Background(), name)
	if err != nil {
		t.Fatalf("SourceByName: %v", err)
	}
	return src.DefaultRunSecrets
}

// TestSourceRunSecretsSetClearAndRender: the operator form records the binding,
// the identity card renders it with the CLI echo, and an empty field clears it.
func TestSourceRunSecretsSetClearAndRender(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	seedRunSecretBinding(t, h, "tree-vault")
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run-secrets",
		url.Values{"run_secrets": {" tree-vault "}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("set run-secrets = %d, want 303", resp.StatusCode)
	}
	if got := defaultRunSecrets(t, h, name); got != "tree-vault" {
		t.Fatalf("DefaultRunSecrets = %q, want tree-vault (trimmed)", got)
	}

	code, page := getBody(t, client, ts.URL+"/sources/"+name)
	if code != http.StatusOK {
		t.Fatalf("/sources/%s = %d", name, code)
	}
	for _, want := range []string{
		`<dt>Run-secrets</dt><dd class="mono">tree-vault</dd>`,
		"exocomp source run-secrets " + name + " tree-vault",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("source detail missing %q:\n%s", want, page)
		}
	}

	resp = postForm(t, client, ts.URL+"/sources/"+name+"/run-secrets",
		url.Values{"run_secrets": {""}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear run-secrets = %d, want 303", resp.StatusCode)
	}
	if got := defaultRunSecrets(t, h, name); got != "" {
		t.Fatalf("DefaultRunSecrets = %q after clear, want empty", got)
	}
	_, page = getBody(t, client, ts.URL+"/sources/"+name)
	if strings.Contains(page, `<dt>Run-secrets</dt>`) {
		t.Errorf("source detail shows a Run-secrets line for an empty default:\n%s", page)
	}
}

// TestSourceRunSecretsRefusesUnknownBinding: a binding with no pairs re-renders
// the page at 422 with the sentence and the typed value, and mutates nothing.
func TestSourceRunSecretsRefusesUnknownBinding(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	name := seedSourceCheckout(t, h)
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/sources/"+name+"/run-secrets",
		url.Values{"run_secrets": {"typo"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown binding = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "That binding has no secrets") {
		t.Errorf("refusal missing the field sentence:\n%s", body)
	}
	if !strings.Contains(body, `value="typo"`) {
		t.Errorf("refusal did not keep the typed value:\n%s", body)
	}
	if got := defaultRunSecrets(t, h, name); got != "" {
		t.Fatalf("a refused binding still landed %q", got)
	}
}

// TestSourceRunSecretsUnknownSource: an unknown source is a 404, never a create.
func TestSourceRunSecretsUnknownSource(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/sources/nope/run-secrets",
		url.Values{"run_secrets": {"x"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-source run-secrets = %d, want 404", resp.StatusCode)
	}
}

// TestSourceRunSecretsRefusesUnauthenticatedAndUnderScoped: the verb sits behind
// the operator floor, and neither refusal mutates the source.
func TestSourceRunSecretsRefusesUnauthenticatedAndUnderScoped(t *testing.T) {
	t.Run("no session is refused and mutates nothing", func(t *testing.T) {
		h := newHarness(t)
		ts := httptest.NewServer(h.server.Handler())
		defer ts.Close()
		name := seedSourceCheckout(t, h)
		seedRunSecretBinding(t, h, "tree-vault")

		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := client.PostForm(ts.URL+"/sources/"+name+"/run-secrets", url.Values{"run_secrets": {"tree-vault"}})
		if err != nil {
			t.Fatalf("POST run-secrets: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("unauthenticated run-secrets = %d, want 303 to /login", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Fatalf("redirected to %q, want /login…", loc)
		}
		if got := defaultRunSecrets(t, h, name); got != "" {
			t.Fatalf("unauthenticated POST set %q", got)
		}
	})

	t.Run("readonly scope is forbidden and mutates nothing", func(t *testing.T) {
		h := newHarness(t)
		ts := httptest.NewServer(h.server.Handler())
		defer ts.Close()
		name := seedSourceCheckout(t, h)
		seedRunSecretBinding(t, h, "tree-vault")
		client := login(t, ts.URL, h.mint(t, "viewer", "readonly"))

		resp := postForm(t, client, ts.URL+"/sources/"+name+"/run-secrets",
			url.Values{"run_secrets": {"tree-vault"}}, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("readonly run-secrets = %d, want 403", resp.StatusCode)
		}
		if got := defaultRunSecrets(t, h, name); got != "" {
			t.Fatalf("readonly POST set %q", got)
		}
	})
}

// TestRulesRenderSourceRunSecretsGuard: a playbook rule naming a source with a
// default binding renders "secrets <binding>" even though the rule declares
// none, so an unattended run's credential reach is legible; a source without a
// default, and a non-playbook rule, render none.
func TestRulesRenderSourceRunSecretsGuard(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	seedEgressSource(t, h, "tree")
	seedEgressSource(t, h, "plain")
	seedRunSecretBinding(t, h, "tree-vault")
	src, err := h.store.SourceByName(context.Background(), "tree")
	if err != nil {
		t.Fatalf("SourceByName: %v", err)
	}
	if _, err := h.store.SetSourceRunSecrets(context.Background(), src.ID, "tree-vault"); err != nil {
		t.Fatalf("SetSourceRunSecrets: %v", err)
	}
	seedEgressRule(t, h, "bound", store.ActionPlaybook, "tree", nil)
	seedEgressRule(t, h, "unbound", store.ActionPlaybook, "plain", nil)
	seedEgressRule(t, h, "shell", store.ActionShell, "tree", nil)

	rows, err := h.server.rules(httptest.NewRequest("GET", "/rules", nil))
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Name] = r.RunSecrets
	}
	if got["bound"] != "secrets tree-vault" {
		t.Errorf("bound rule guard = %q, want %q", got["bound"], "secrets tree-vault")
	}
	if got["unbound"] != "" || got["shell"] != "" {
		t.Errorf("guards for unbound/shell = %q/%q, want none", got["unbound"], got["shell"])
	}

	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	code, page := getBody(t, client, ts.URL+"/rules")
	if code != http.StatusOK || !strings.Contains(page, "secrets tree-vault") {
		t.Errorf("/rules = %d, missing the secrets guard", code)
	}
}

// TestApprovalPageRendersRunSecretBinding: an approver sees the binding the
// released job receives, by name; a job with none renders no block.
func TestApprovalPageRendersRunSecretBinding(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	_, bound := h.gatedJobSpec(t, time.Minute, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual",
		Argv:             []string{"/bin/sh", "-c", "echo ran"},
		RunSecretBinding: "tree-vault",
	})
	code, page := getBody(t, client, ts.URL+"/approvals/"+itoa64(bound.ID))
	if code != http.StatusOK {
		t.Fatalf("approval surface status = %d, want 200", code)
	}
	if !strings.Contains(page, `<span class="keyword">Run-secrets</span>`) ||
		!strings.Contains(page, "tree-vault") {
		t.Errorf("approval surface missing the run-secrets binding:\n%s", page)
	}

	_, plain := h.gatedJob(t, time.Minute)
	_, page = getBody(t, client, ts.URL+"/approvals/"+itoa64(plain.ID))
	if strings.Contains(page, `<span class="keyword">Run-secrets</span>`) {
		t.Errorf("approval surface shows a Run-secrets block for a job with none:\n%s", page)
	}
}
