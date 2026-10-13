package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/web"
)

// ntfyTestStore opens a store and envelope over a fresh data dir, the wiring
// the seam runs under in both the daemon and the standalone web process.
func ntfyTestStore(t *testing.T) (*store.Store, *secrets.Envelope) {
	t.Helper()
	dir := initForSource(t)
	st, err := store.Open(filepath.Join(dir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("open envelope: %v", err)
	}
	t.Cleanup(func() { _ = env.Close() })
	t.Cleanup(func() { _ = st.Close() })
	return st, env
}

// sealNtfy seals one ntfy secret with its real value, the way the setup wizard
// and the sealed-value writes do.
func sealNtfy(t *testing.T, st *store.Store, env *secrets.Envelope, name, value string) {
	t.Helper()
	sealed, err := env.Seal(name, "generic", []byte(value))
	if err != nil {
		t.Fatalf("seal %s: %v", name, err)
	}
	if _, err := st.CreateSecret(context.Background(), store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		AAD: sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

// TestNtfySeamStateSelfHosted is the ON judgment: a self-hosted server needs
// no token, so the sealed channel is not refused.
func TestNtfySeamStateSelfHosted(t *testing.T) {
	st, env := ntfyTestStore(t)
	seam := newNtfySeam(st, env)

	got, err := seam.State(context.Background())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if got.URLSealed || got.TopicSealed || got.TokenSealed || got.Refused || got.ServerClass != "" {
		t.Fatalf("unsealed state = %+v, want all-false presence, no class, not refused", got)
	}

	sealNtfy(t, st, env, notify.SecretNtfyURL, "https://push.lan")
	sealNtfy(t, st, env, notify.SecretNtfyTopic, "exocomp-7f3a")
	sealNtfy(t, st, env, notify.SecretNtfyToken, "tk_secret")
	got, err = seam.State(context.Background())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !got.URLSealed || !got.TopicSealed || !got.TokenSealed || got.Refused || got.ServerClass != "self-hosted" {
		t.Fatalf("self-hosted state = %+v, want sealed, self-hosted, not refused", got)
	}
}

// TestNtfySeamFromSettings is the settings path: the URL and topic are ordinary
// settings (no sealed rows), the token is sealed, and the seam reads the same
// values the transport does — present, self-hosted, and a test push that lands.
func TestNtfySeamFromSettings(t *testing.T) {
	st, env := ntfyTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 4<<10))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	if err := st.PutSetting(ctx, store.SettingNtfyURL, srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting(ctx, store.SettingNtfyTopic, "exocomp-settings"); err != nil {
		t.Fatal(err)
	}
	sealNtfy(t, st, env, notify.SecretNtfyToken, "tk_secret")

	seam := newNtfySeam(st, env)
	got, err := seam.State(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !got.URLSealed || !got.TopicSealed || !got.TokenSealed || got.Refused || got.ServerClass != "self-hosted" {
		t.Fatalf("settings state = %+v, want present, self-hosted, not refused", got)
	}
	if err := seam.TestPush(ctx, "nibbler"); err != nil {
		t.Fatalf("test push from settings: %v", err)
	}
}

// TestNtfySeamSettingWinsOverLegacySealed is the precedence gate: a legacy
// sealed ntfy/url or ntfy/topic (from before those became settings) must not
// shadow the setting — the value the UI shows is the value the transport uses.
func TestNtfySeamSettingWinsOverLegacySealed(t *testing.T) {
	st, env := ntfyTestStore(t)
	ctx := context.Background()
	sealNtfy(t, st, env, notify.SecretNtfyURL, "https://old.lan")
	sealNtfy(t, st, env, notify.SecretNtfyTopic, "old-topic")
	if err := st.PutSetting(ctx, store.SettingNtfyURL, "https://new.lan"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting(ctx, store.SettingNtfyTopic, "new-topic"); err != nil {
		t.Fatal(err)
	}
	seam := newNtfySeam(st, env)
	url, err := seam.look(ctx, notify.SecretNtfyURL)
	if err != nil || string(url) != "https://new.lan" {
		t.Fatalf("resolved url = %q (%v), want the setting", url, err)
	}
	topic, err := seam.look(ctx, notify.SecretNtfyTopic)
	if err != nil || string(topic) != "new-topic" {
		t.Fatalf("resolved topic = %q (%v), want the setting", topic, err)
	}
}

// TestNtfySeamLegacySealedFallback: with no setting, a legacy sealed URL/topic
// still resolves, so an install from before the settings move keeps working.
func TestNtfySeamLegacySealedFallback(t *testing.T) {
	st, env := ntfyTestStore(t)
	ctx := context.Background()
	sealNtfy(t, st, env, notify.SecretNtfyURL, "https://old.lan")
	sealNtfy(t, st, env, notify.SecretNtfyTopic, "old-topic")
	seam := newNtfySeam(st, env)
	url, err := seam.look(ctx, notify.SecretNtfyURL)
	if err != nil || string(url) != "https://old.lan" {
		t.Fatalf("legacy url = %q (%v), want the sealed fallback", url, err)
	}
}

// TestNtfySeamStateRefusesPublicWithoutToken is the NewNtfy judgment
// re-derived from the sealed secrets: a public topic without a token is
// refused, and sealing the token clears the refusal.
func TestNtfySeamStateRefusesPublicWithoutToken(t *testing.T) {
	st, env := ntfyTestStore(t)
	sealNtfy(t, st, env, notify.SecretNtfyURL, "https://ntfy.sh")
	sealNtfy(t, st, env, notify.SecretNtfyTopic, "exocomp-7f3a")
	seam := newNtfySeam(st, env)

	got, err := seam.State(context.Background())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !got.URLSealed || !got.TopicSealed || got.TokenSealed {
		t.Fatalf("presence = %+v, want url+topic sealed, token missing", got)
	}
	if !got.Refused || got.ServerClass != "public" {
		t.Fatalf("judgment = %+v, want refused on the public server", got)
	}

	sealNtfy(t, st, env, notify.SecretNtfyToken, "tk_secret")
	got, _ = seam.State(context.Background())
	if got.Refused || got.ServerClass != "public" {
		t.Fatalf("with token = %+v, want public and not refused", got)
	}
}

// TestNtfySeamTestPushDelivers sends one test push through the same LoadNtfy
// transport the daemon builds, against a local server.
func TestNtfySeamTestPushDelivers(t *testing.T) {
	st, env := ntfyTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 4<<10))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sealNtfy(t, st, env, notify.SecretNtfyURL, srv.URL)
	sealNtfy(t, st, env, notify.SecretNtfyTopic, "exocomp-test")
	seam := newNtfySeam(st, env)
	if err := seam.TestPush(context.Background(), "nibbler"); err != nil {
		t.Fatalf("test push: %v", err)
	}
}

// TestNtfySeamTestPushRefusal: a public topic without a token fails the test
// push with the refusal wrap the drawer classifies as "the server refused the
// token".
func TestNtfySeamTestPushRefusal(t *testing.T) {
	st, env := ntfyTestStore(t)
	sealNtfy(t, st, env, notify.SecretNtfyURL, "https://ntfy.sh")
	sealNtfy(t, st, env, notify.SecretNtfyTopic, "exocomp-7f3a")
	seam := newNtfySeam(st, env)

	err := seam.TestPush(context.Background(), "nibbler")
	if err == nil {
		t.Fatal("public topic without a token must refuse the test push")
	}
	if !errors.Is(err, web.ErrNtfyRefused) {
		t.Fatalf("test push error = %v, want the refusal wrap", err)
	}
}

// TestNtfySeamTestPushNoChannel: nothing sealed is a nil transport, and the
// test push says so rather than pretending to deliver.
func TestNtfySeamTestPushNoChannel(t *testing.T) {
	st, env := ntfyTestStore(t)
	seam := newNtfySeam(st, env)
	if err := seam.TestPush(context.Background(), "nibbler"); err == nil {
		t.Fatal("no channel must refuse the test push")
	}
}
