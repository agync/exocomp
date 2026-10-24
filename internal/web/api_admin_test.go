package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

type apiCall struct {
	name, method, path, body string
	status                   int
	contains                 string
}

func runAPICalls(t *testing.T, base, token string, calls []apiCall) {
	t.Helper()
	for _, c := range calls {
		var code int
		var body string
		switch c.method {
		case http.MethodPost:
			code, _, body = bearerPost(t, base, c.path, token, c.body, nil)
		case http.MethodPut:
			code, body = bearerPut(t, base, c.path, token, c.body)
		case http.MethodDelete:
			code, body = bearerDelete(t, base, c.path, token)
		default:
			code, _, body = bearerGet(t, base, c.path, token)
		}
		if code != c.status || !strings.Contains(body, c.contains) {
			t.Fatalf("%s: %s %s = %d %s, want %d containing %q", c.name, c.method, c.path, code, body, c.status, c.contains)
		}
	}
}

func TestAPISecretAndRunSecretRoutes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")

	runAPICalls(t, ts.URL, op, []apiCall{
		{"seal", http.MethodPost, "/api/v1/secrets", `{"name":"team/api","value":"YWJjMTIz","expires_at":"2099-01-01T00:00:00Z"}`,
			http.StatusOK, `"bytes":6`},
		{"duplicate", http.MethodPost, "/api/v1/secrets", `{"name":"team/api","value":"eA=="}`, http.StatusConflict, "duplicate"},
		{"bad name", http.MethodPost, "/api/v1/secrets", `{"name":"has space","value":"eA=="}`, http.StatusBadRequest, "name"},
		{"no value", http.MethodPost, "/api/v1/secrets", `{"name":"empty"}`, http.StatusBadRequest, "empty value"},
		{"bad expiry", http.MethodPost, "/api/v1/secrets", `{"name":"x","value":"dg==","expires_at":"720h"}`,
			http.StatusBadRequest, "RFC3339"},
		{"generate", http.MethodPost, "/api/v1/secrets", `{"name":"minted","generate":true}`, http.StatusOK, `"source":"generated"`},
		{"replace", http.MethodPut, "/api/v1/secrets/team%2Fapi", `{"value":"eHl6"}`, http.StatusOK, `"rotated":false`},
		{"rotate", http.MethodPut, "/api/v1/secrets/minted", `{}`, http.StatusOK, `"regenerated":true`},
		{"replace unknown", http.MethodPut, "/api/v1/secrets/nope", `{"value":"dg=="}`, http.StatusNotFound, "not_found"},
		{"bind", http.MethodPost, "/api/v1/run-secrets/api", `{"env_name":"API_TOKEN","secret":"team/api"}`,
			http.StatusOK, `"env_name":"API_TOKEN"`},
		{"bind bad env", http.MethodPost, "/api/v1/run-secrets/api", `{"env_name":"1BAD","secret":"team/api"}`,
			http.StatusBadRequest, "environment variable"},
		{"bind unknown secret", http.MethodPost, "/api/v1/run-secrets/api", `{"env_name":"OTHER","secret":"nope"}`,
			http.StatusNotFound, "nope"},
		{"bindings", http.MethodGet, "/api/v1/run-secrets", "", http.StatusOK, `["api"]`},
		{"pairs", http.MethodGet, "/api/v1/run-secrets/api", "", http.StatusOK, `"secret":"team/api"`},
		{"rm bound", http.MethodDelete, "/api/v1/secrets/team%2Fapi", "", http.StatusConflict, "binding api env API_TOKEN"},
		{"rm forced", http.MethodDelete, "/api/v1/secrets/team%2Fapi?force=true", "", http.StatusOK, `"forced_bindings":1`},
		{"unbind", http.MethodDelete, "/api/v1/run-secrets/api", "", http.StatusOK, `"binding":"api"`},
	})

	row, err := h.store.GetSecret(context.Background(), "minted")
	if err != nil || row.Source != store.SecretSourceGenerated {
		t.Fatalf("minted = %+v (%v)", row, err)
	}
}

// No answer carries a sealed value.
func TestAPISecretValueNeverComesBack(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/secrets", `{"name":"k","value":"cGw0MW50M3h0LXZhbHVl"}`},
		{http.MethodPut, "/api/v1/secrets/k", `{"value":"cGw0MW50M3h0LW90aGVy"}`},
	} {
		var body string
		if c.method == http.MethodPost {
			_, _, body = bearerPost(t, ts.URL, c.path, op, c.body, nil)
		} else {
			_, body = bearerPut(t, ts.URL, c.path, op, c.body)
		}
		if strings.Contains(body, "pl41nt3xt") || strings.Contains(body, "cGw0MW50M3h0") {
			t.Fatalf("%s %s echoed the value: %s", c.method, c.path, body)
		}
	}
	_, _, list := bearerGet(t, ts.URL, "/api/v1/secrets", op)
	if strings.Contains(list, "pl41nt3xt") {
		t.Fatalf("the secret list carries a value: %s", list)
	}
}

func TestAPISourceRoutes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")
	root := t.TempDir()

	runAPICalls(t, ts.URL, op, []apiCall{
		{"add path", http.MethodPost, "/api/v1/sources", `{"name":"infra","path":"` + root + `"}`,
			http.StatusOK, `"kind":"path"`},
		{"duplicate", http.MethodPost, "/api/v1/sources", `{"name":"infra","path":"/srv"}`, http.StatusConflict, "duplicate"},
		{"both kinds", http.MethodPost, "/api/v1/sources", `{"name":"x","path":"/srv","url":"https://forge.invalid/x.git"}`,
			http.StatusBadRequest, "exactly one"},
		{"signed without signers", http.MethodPost, "/api/v1/sources", `{"name":"x","url":"https://forge.invalid/x.git","signed":true}`,
			http.StatusBadRequest, "allowed_signers"},
		{"bad name", http.MethodPost, "/api/v1/sources", `{"name":"../x","path":"/srv"}`, http.StatusBadRequest, ""},
		{"add git", http.MethodPost, "/api/v1/sources", `{"name":"fleet","url":"git@forge.invalid:me/fleet.git"}`,
			http.StatusOK, `"public_key":"ssh-ed25519 `},
		{"egress", http.MethodPut, "/api/v1/sources/infra/egress", `{"ports":[443]}`, http.StatusOK, `"extra_egress_ports":[443]`},
		{"bad egress", http.MethodPut, "/api/v1/sources/infra/egress", `{"ports":[70000]}`, http.StatusBadRequest, ""},
		{"unbound run-secrets", http.MethodPut, "/api/v1/sources/infra/run-secrets", `{"binding":"none"}`,
			http.StatusBadRequest, "no pairs"},
		{"sync a path source", http.MethodPost, "/api/v1/sources/infra/sync", "", http.StatusBadRequest, "only git sources sync"},
		{"rm", http.MethodDelete, "/api/v1/sources/infra", "", http.StatusOK, `"name":"infra"`},
		{"rm unknown", http.MethodDelete, "/api/v1/sources/infra", "", http.StatusNotFound, "not_found"},
	})

	fleet, err := h.store.SourceByName(context.Background(), "fleet")
	if err != nil || fleet.DeployKeySecret == "" {
		t.Fatalf("git source = %+v (%v), want a sealed deploy key", fleet, err)
	}
	if _, err := h.store.GetSecret(context.Background(), fleet.DeployKeySecret); err != nil {
		t.Fatalf("deploy key not sealed: %v", err)
	}

	// The deploy key exocomp generated is neither bound nor replaced over the API.
	runAPICalls(t, ts.URL, op, []apiCall{
		{"bind a deploy key", http.MethodPost, "/api/v1/run-secrets/leak",
			`{"env_name":"KEY","secret":"` + fleet.DeployKeySecret + `"}`, http.StatusForbidden, "--local"},
		{"replace a deploy key", http.MethodPut, "/api/v1/secrets/source%2Ffleet%2Fdeploy-key", `{"value":"eA=="}`,
			http.StatusForbidden, "--local"},
	})
	if pairs, _ := h.store.RunSecretsByBinding(context.Background(), "leak"); len(pairs) != 0 {
		t.Fatal("a deploy key was bound over the API")
	}
}

func TestAPIAdminRoutesNeedAnOperatorBearer(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	for _, scope := range []string{"readonly", "approve"} {
		token := h.mint(t, scope, scope)
		runAPICalls(t, ts.URL, token, []apiCall{
			{"source add", http.MethodPost, "/api/v1/sources", `{"name":"infra","path":"/srv"}`, http.StatusForbidden, "operator"},
			{"secret add", http.MethodPost, "/api/v1/secrets", `{"name":"k","value":"dg=="}`, http.StatusForbidden, "operator"},
			{"bind", http.MethodPost, "/api/v1/run-secrets/api", `{"env_name":"A","secret":"k"}`, http.StatusForbidden, "operator"},
		})
	}
	srcs, _ := h.store.ListSources(context.Background())
	secs, _ := h.store.ListSecrets(context.Background())
	if len(srcs) != 0 || len(secs) != 0 {
		t.Fatalf("a refused call changed the store: %d sources, %d secrets", len(srcs), len(secs))
	}
}

// A process that holds no envelope cannot seal, so the sealing routes answer
// 503 and store nothing.
func TestAPISealingNeedsTheEnvelope(t *testing.T) {
	h := newHarness(t)
	h.server.sealer = nil
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")

	runAPICalls(t, ts.URL, op, []apiCall{
		{"secret add", http.MethodPost, "/api/v1/secrets", `{"name":"k","value":"dg=="}`, http.StatusServiceUnavailable, "envelope"},
		{"git source", http.MethodPost, "/api/v1/sources", `{"name":"fleet","url":"git@forge.invalid:me/fleet.git"}`,
			http.StatusServiceUnavailable, "envelope"},
	})
	srcs, _ := h.store.ListSources(context.Background())
	if len(srcs) != 0 {
		t.Fatal("a git source was recorded without its deploy key")
	}
}

// The names and kinds exocomp uses for its own transport keys are out of the
// API's reach, whether or not a key is sealed under them yet, so a name cannot
// be bound early, squatted, or freed for exocomp to fill later.
func TestAPITransportKeyNamesAreReserved(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")
	ctx := context.Background()

	sealed, err := h.env.Seal("fleet/ssh", "fleet-ssh-key", []byte("PRIVATE"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{Name: sealed.Name, Kind: sealed.Kind,
		Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext, AAD: sealed.Name + "|" + sealed.Kind}); err != nil {
		t.Fatal(err)
	}

	runAPICalls(t, ts.URL, op, []apiCall{
		{"seal under a deploy-key name", http.MethodPost, "/api/v1/secrets",
			`{"name":"source/newsrc/deploy-key","value":"eA=="}`, http.StatusForbidden, "--local"},
		{"seal the fleet name", http.MethodPost, "/api/v1/secrets", `{"name":"fleet/ssh","value":"eA=="}`,
			http.StatusForbidden, "--local"},
		{"seal a transport kind", http.MethodPost, "/api/v1/secrets", `{"name":"k","kind":"deploy-key","value":"eA=="}`,
			http.StatusForbidden, "--local"},
		{"bind a reserved name not yet sealed", http.MethodPost, "/api/v1/run-secrets/b",
			`{"env_name":"K","secret":"source/newsrc/deploy-key"}`, http.StatusForbidden, "--local"},
		{"bind the fleet key", http.MethodPost, "/api/v1/run-secrets/b", `{"env_name":"K","secret":"fleet/ssh"}`,
			http.StatusForbidden, "--local"},
		{"replace the fleet key", http.MethodPut, "/api/v1/secrets/fleet%2Fssh", `{"value":"eA=="}`,
			http.StatusForbidden, "--local"},
		{"remove the fleet key", http.MethodDelete, "/api/v1/secrets/fleet%2Fssh", "", http.StatusForbidden, "--local"},
		{"seal under an output name", http.MethodPost, "/api/v1/secrets",
			`{"name":"tofu-output/edge/kubeconfig","value":"eA=="}`, http.StatusForbidden, "--local"},
		{"seal an output kind", http.MethodPost, "/api/v1/secrets", `{"name":"k","kind":"ansible-output","value":"eA=="}`,
			http.StatusForbidden, "--local"},
		{"add to an output binding", http.MethodPost, "/api/v1/run-secrets/ab:boot", `{"env_name":"K","secret":"k"}`,
			http.StatusForbidden, "--local"},
		{"remove an output binding", http.MethodDelete, "/api/v1/run-secrets/tf:edge", "", http.StatusForbidden, "--local"},
	})
	if got, err := h.store.GetSecret(ctx, "fleet/ssh"); err != nil || got.Kind != "fleet-ssh-key" {
		t.Fatalf("fleet key = %+v (%v), want it untouched", got, err)
	}
	if pairs, _ := h.store.RunSecretsByBinding(ctx, "b"); len(pairs) != 0 {
		t.Fatal("a reserved name was bound")
	}
}

func TestAPIAdminBodyAndInputRefusals(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")

	runAPICalls(t, ts.URL, op, []apiCall{
		{"misspelt field", http.MethodPost, "/api/v1/secrets", `{"name":"k","value":"eA==","expires":"720h"}`,
			http.StatusBadRequest, "expires"},
		{"value too large", http.MethodPost, "/api/v1/secrets",
			`{"name":"k","value":"` + strings.Repeat("A", maxAdminBody) + `"}`, http.StatusRequestEntityTooLarge, ""},
		{"trailing JSON", http.MethodPost, "/api/v1/secrets", `{"name":"k","value":"eA=="} {"name":"j"}`,
			http.StatusBadRequest, "more than one JSON value"},
		{"path covers the data dir", http.MethodPost, "/api/v1/sources", `{"name":"root","path":"/"}`,
			http.StatusBadRequest, "ancestor of the data dir"},
		{"relative path leaves the data volume", http.MethodPost, "/api/v1/sources", `{"name":"up","path":"../x"}`,
			http.StatusBadRequest, ""},
		{"url not git", http.MethodPost, "/api/v1/sources", `{"name":"x","url":"/srv/private.git"}`,
			http.StatusBadRequest, "path source"},
		{"writable git", http.MethodPost, "/api/v1/sources", `{"name":"x","url":"https://forge.invalid/x.git","writable":true}`,
			http.StatusBadRequest, "writable"},
		{"ref as an option", http.MethodPost, "/api/v1/sources", `{"name":"x","url":"https://forge.invalid/x.git","ref":"--upload-pack=x"}`,
			http.StatusBadRequest, "git ref"},
		{"add git", http.MethodPost, "/api/v1/sources", `{"name":"fleet","url":"https://127.0.0.1:1/fleet.git"}`,
			http.StatusOK, ""},
		{"sync ref as an option", http.MethodPost, "/api/v1/sources/fleet/sync", `{"ref":"-x"}`, http.StatusBadRequest, "git ref"},
	})

	// The fetch fails against a closed loopback port; the answer names the log,
	// not git's output.
	code, _, body := bearerPost(t, ts.URL, "/api/v1/sources/fleet/sync", op, "", nil)
	if code != http.StatusBadGateway || !strings.Contains(body, "daemon log") ||
		strings.Contains(body, "127.0.0.1") || strings.Contains(body, "fatal") {
		t.Fatalf("sync failure = %d %s, want a 502 that keeps git's output out", code, body)
	}
}

// A source's run-secrets default cannot name a binding exocomp fills with
// sealed outputs.
func TestAPISourceDefaultRefusesOutputBindings(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")
	if _, err := h.store.CreateSource(context.Background(), store.Source{Name: "infra", Kind: store.SourcePath,
		URI: t.TempDir(), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runAPICalls(t, ts.URL, op, []apiCall{
		{"default to tf:", http.MethodPut, "/api/v1/sources/infra/run-secrets", `{"binding":"tf:edge"}`,
			http.StatusForbidden, "--local"},
	})
}

// A source name whose deploy key an earlier source left sealed is a conflict
// about the key, not a duplicate source.
func TestAPIGitSourceOverAnOrphanKey(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")

	runAPICalls(t, ts.URL, op, []apiCall{
		{"add", http.MethodPost, "/api/v1/sources", `{"name":"fleet","url":"git@forge.invalid:me/fleet.git"}`, http.StatusOK, ""},
		{"rm", http.MethodDelete, "/api/v1/sources/fleet", "", http.StatusOK, ""},
		{"add again", http.MethodPost, "/api/v1/sources", `{"name":"fleet","url":"git@forge.invalid:me/fleet.git"}`,
			http.StatusConflict, "already sealed"},
	})
	if _, err := h.store.SourceByName(context.Background(), "fleet"); err == nil {
		t.Fatal("the source row was kept after the key collision")
	}
}
