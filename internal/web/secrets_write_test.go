package web

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// syncLog is a mutex-guarded writer so a test can read the server's log while
// the handler goroutine may still be writing (safe under -race).
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// serverWithLog builds a web Server over the harness store with a captured
// logger, so a test can assert the write path's log copy.
func serverWithLog(t *testing.T, h *harness, log *syncLog) *Server {
	t.Helper()
	srv, err := New(Options{
		Store: h.store, Deriver: h.env, Decider: h.engine, Engine: h.engine,
		Plans: h.env, Sealer: h.env, Version: "test", DataDir: h.dir,
		Logger: slog.New(slog.NewTextHandler(log, nil)),
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv
}

// TestSecretRmRequiresConfirmation: the hidden confirmed=1 field is the
// server-side gate the secrets Delete form's dialog re-submission carries —
// a session alone must not be able to destroy a sealed secret. A
// confirmed-less rm re-renders the surface at 422 with the refusal on the row
// (forms/SPEC.md, never a flash) and the secret survives; with confirmed=1
// it lands.

func TestSecretRmRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	op := h.mint(t, "abn", "operator")
	client := login(t, ts.URL, op)
	if _, err := h.store.CreateSecret(ctx, store.SecretRow{
		Name: "ssh-key", Kind: "generic", Source: store.SecretSourceUser,
		Nonce: []byte("NONCE-SENTINEL-D"), Ciphertext: []byte("CIPHER-SENTINEL-D"),
		AAD: "ssh-key|generic",
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/secrets/ssh-key/rm", url.Values{}, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("rm without confirmed = %d, want 422 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Nothing was removed — confirm the removal in the dialog.") {
		t.Errorf("confirmed-less rm refusal missing the on-row sentence")
	}
	if _, err := h.store.GetSecret(ctx, "ssh-key"); err != nil {
		t.Fatalf("secret vanished despite refused rm: %v", err)
	}

	resp = postForm(t, client, ts.URL+"/secrets/ssh-key/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm with confirmed = %d, want 303", resp.StatusCode)

	}
	if _, err := h.store.GetSecret(ctx, "ssh-key"); err == nil {
		t.Fatal("secret still present after confirmed rm")
	}
}

// TestSecretChangeSubmitRefusalNamesSource pins the whole-submit refusal and
// the success flash to the honest verb: rotate for a generated secret, replace
// for an operator-supplied one.
func TestSecretChangeSubmitRefusalNamesSource(t *testing.T) {
	if got := secretChangeSubmitRefusal(store.SecretSourceGenerated); !strings.Contains(got, "rotate") {
		t.Errorf("generated refusal = %q, want rotate", got)
	}
	if got := secretChangeSubmitRefusal(store.SecretSourceUser); !strings.Contains(got, "replace") {
		t.Errorf("user refusal = %q, want replace", got)
	}
	if got := secretChangePastVerb(store.SecretSourceGenerated); got != "rotated" {
		t.Errorf("generated flash verb = %q, want rotated", got)
	}
	if got := secretChangePastVerb(store.SecretSourceUser); got != "replaced" {
		t.Errorf("user flash verb = %q, want replaced", got)
	}
	if got := secretChangeVerb(store.SecretSourceGenerated); got != "rotate" {
		t.Errorf("generated log verb = %q, want rotate", got)
	}
	if got := secretChangeVerb(store.SecretSourceUser); got != "replace" {
		t.Errorf("user log verb = %q, want replace", got)
	}
}

// TestSecretChangeFlashNamesSource drives the unchanged /secrets/{name}/rotate
// route for both sources and asserts the success flash branches: a user secret
// is replaced, a generated one is rotated.
func TestSecretChangeFlashNamesSource(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	tok := h.mint(t, "laptop", "operator")
	op := login(t, ts.URL, tok)
	ctx := context.Background()
	for _, sec := range []store.SecretRow{
		{Name: "supplied", Kind: "generic", Source: store.SecretSourceUser, Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "supplied|generic"},
		{Name: "minted", Kind: "generic", Source: store.SecretSourceGenerated, Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: "minted|generic"},
	} {
		if _, err := h.store.CreateSecret(ctx, sec); err != nil {
			t.Fatalf("CreateSecret %s: %v", sec.Name, err)
		}
	}

	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"supplied", url.Values{"value": {"new"}, "confirm_token": {tok}}, "replaced supplied"},
		{"minted", url.Values{"generate": {"1"}, "confirm_token": {tok}}, "rotated minted"},
	} {
		resp := postForm(t, op, ts.URL+"/secrets/"+tc.name+"/rotate", tc.form, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s change = %d, want 303", tc.name, resp.StatusCode)
		}
		var flash string
		for _, c := range resp.Cookies() {
			if c.Name == flashCookie {
				flash = c.Value
			}
		}
		if flash != tc.want {
			t.Errorf("%s flash = %q, want %q", tc.name, flash, tc.want)
		}
	}
}

// TestSecretChangeLogNamesSource pins the write path's log copy to the source's
// honest verb: an operator-supplied secret is replaced, only a generated one is
// rotated. The MUTATION — restoring the hardcoded "web: secret rotate" — reds
// this test for the replace source.
func TestSecretChangeLogNamesSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		form   url.Values
		verb   string
	}{
		{"supplied", store.SecretSourceUser, url.Values{"value": {"new"}}, "web: secret replace"},
		{"minted", store.SecretSourceGenerated, url.Values{"generate": {"1"}}, "web: secret rotate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var log syncLog
			srv := serverWithLog(t, h, &log)
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			tok := h.mint(t, "laptop", "operator")
			op := login(t, ts.URL, tok)
			ctx := context.Background()
			if _, err := h.store.CreateSecret(ctx, store.SecretRow{
				Name: tc.name, Kind: "generic", Source: tc.source,
				Nonce: []byte("N"), Ciphertext: []byte("C"), AAD: tc.name + "|generic",
			}); err != nil {
				t.Fatalf("CreateSecret: %v", err)
			}
			form := url.Values{}
			for k, v := range tc.form {
				form[k] = v
			}
			form.Set("confirm_token", tok)
			resp := postForm(t, op, ts.URL+"/secrets/"+tc.name+"/rotate", form, nil)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("%s change = %d, want 303", tc.name, resp.StatusCode)
			}
			if got := log.String(); !strings.Contains(got, `msg="`+tc.verb+`"`) {
				t.Errorf("log must carry %q, got:\n%s", "web: secret "+secretChangeVerb(tc.source), got)
			}
			if tc.verb == "web: secret replace" && strings.Contains(log.String(), "web: secret rotate") {
				t.Errorf("a user secret's change must not log the rotate verb:\n%s", log.String())
			}
		})
	}
}
