package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/notify"
)

type goalertRecord struct {
	path  string
	token string
	body  map[string]any
}

func goalertServer(t *testing.T, status int) (*httptest.Server, *[]goalertRecord) {
	t.Helper()
	var seen []goalertRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read publish body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode publish body %q: %v", raw, err)
		}
		seen = append(seen, goalertRecord{
			path:  r.URL.Path,
			token: r.URL.Query().Get("token"),
			body:  body,
		})
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestGoAlertOpensAnIncidentWithDedup(t *testing.T) {
	t.Parallel()
	srv, seen := goalertServer(t, http.StatusOK)

	transport, err := notify.NewGoAlert(notify.GoAlertOptions{
		BaseURL: srv.URL, Key: "tok_secret", Client: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new goalert: %v", err)
	}

	err = notify.New(slog.New(slog.DiscardHandler), transport).Notify(context.Background(),
		notify.Notification{
			Title:    "job failed: workspace:prod",
			Body:     "Job 7 (tofu) ended failed. Last error: apply exited 1",
			Priority: notify.PriorityHigh,
			Source:   "workspace:prod",
			Host:     "prod",
		})
	if err != nil {
		t.Fatalf("notify: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("server saw %d publishes, want 1", len(*seen))
	}
	got := (*seen)[0]
	if got.path != "/api/v2/generic/incoming" {
		t.Fatalf("request path = %q, want the generic incoming path", got.path)
	}
	if got.token != "tok_secret" {
		t.Fatalf("token param = %q, want the integration key", got.token)
	}
	if got.body["summary"] != "job failed: workspace:prod" {
		t.Fatalf("summary = %v", got.body["summary"])
	}
	if got.body["details"] != "Job 7 (tofu) ended failed. Last error: apply exited 1" {
		t.Fatalf("details = %v", got.body["details"])
	}
	if got.body["dedup"] != "workspace:prod/prod" {
		t.Fatalf("dedup = %v, want it derived from source and host", got.body["dedup"])
	}
	if got.body["action"] != "open" {
		t.Fatalf("action = %v, want open for a firing notification", got.body["action"])
	}
}

// TestGoAlertClosesOnResolved is the resolve path: a notification marked
// Resolved closes the incident its firing opened, and closes it under the same
// dedup so GoAlert matches the two.
func TestGoAlertClosesOnResolved(t *testing.T) {
	t.Parallel()
	srv, seen := goalertServer(t, http.StatusOK)

	transport, err := notify.NewGoAlert(notify.GoAlertOptions{
		BaseURL: srv.URL, Key: "tok_secret", Client: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new goalert: %v", err)
	}

	err = transport.Publish(context.Background(), notify.Notification{
		Title:    "cleared",
		Source:   "converge",
		Host:     "space-a",
		Resolved: true,
	})
	if err != nil {
		t.Fatalf("publish resolve: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("server saw %d publishes, want 1", len(*seen))
	}
	got := (*seen)[0]
	if got.body["action"] != "close" {
		t.Fatalf("action = %v, want close for a resolved notification", got.body["action"])
	}
	if got.body["dedup"] != "converge/space-a" {
		t.Fatalf("dedup = %v, want the same key the firing used", got.body["dedup"])
	}
}

// TestGoAlertNamespacePrefixesDedup pins that an operator-set namespace scopes
// every incident key, so two instances sharing one integration do not collide.
func TestGoAlertNamespacePrefixesDedup(t *testing.T) {
	t.Parallel()
	srv, seen := goalertServer(t, http.StatusOK)

	transport, err := notify.NewGoAlert(notify.GoAlertOptions{
		BaseURL: srv.URL, Key: "tok_secret", Namespace: "homelab", Client: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new goalert: %v", err)
	}
	if err := transport.Publish(context.Background(), notify.Notification{
		Title: "x", Source: "converge", Host: "space-a",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if (*seen)[0].body["dedup"] != "homelab:converge/space-a" {
		t.Fatalf("dedup = %v, want the namespace prefix", (*seen)[0].body["dedup"])
	}
}

func TestGoAlertReportsAServerRefusal(t *testing.T) {
	t.Parallel()
	srv, _ := goalertServer(t, http.StatusInternalServerError)

	transport, err := notify.NewGoAlert(notify.GoAlertOptions{
		BaseURL: srv.URL, Key: "tok_secret", Client: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new goalert: %v", err)
	}
	err = transport.Publish(context.Background(), notify.Notification{Title: "hi"})
	if err == nil {
		t.Fatal("publish to a refusing server: want an error, got none")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %v, want the server's status in it", err)
	}
	if strings.Contains(err.Error(), "tok_secret") {
		t.Fatalf("error leaks the integration key: %v", err)
	}
}

// TestGoAlertErrorNeverLeaksTheKey is invariant 4 for the transport path: a
// connection failure returns a *url.Error whose URL carries the token, and the
// transport must strip it before returning.
func TestGoAlertErrorNeverLeaksTheKey(t *testing.T) {
	t.Parallel()
	srv, _ := goalertServer(t, http.StatusOK)
	client := srv.Client()
	base := srv.URL
	srv.Close() // now unreachable: client.Do returns a *url.Error carrying the URL

	transport, err := notify.NewGoAlert(notify.GoAlertOptions{
		BaseURL: base, Key: "tok_secret_key", Client: client,
	})
	if err != nil {
		t.Fatalf("new goalert: %v", err)
	}
	err = transport.Publish(context.Background(), notify.Notification{Title: "hi"})
	if err == nil {
		t.Fatal("publish to an unreachable server: want an error, got none")
	}
	if strings.Contains(err.Error(), "tok_secret_key") {
		t.Fatalf("transport error leaks the integration key: %v", err)
	}
}

func TestLoadGoAlertFromSealedSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sealed := map[string]string{
		notify.SecretGoAlertURL: "https://goalert.lan\n",
		notify.SecretGoAlertKey: "tok_secret\n",
	}
	look := func(_ context.Context, name string) ([]byte, error) {
		v, ok := sealed[name]
		if !ok {
			return nil, errNotSealed
		}
		return []byte(v), nil
	}

	transport, err := notify.LoadGoAlert(ctx, look)
	if err != nil {
		t.Fatalf("load goalert: %v", err)
	}
	if transport == nil {
		t.Fatal("load goalert returned no transport")
	}
	if got := transport.Endpoint(); got != "https://goalert.lan/api/v2/generic/incoming" {
		t.Fatalf("endpoint = %q", got)
	}

	// No configuration at all is a working daemon with a silent phone.
	empty, err := notify.LoadGoAlert(ctx, func(context.Context, string) ([]byte, error) {
		return nil, errNotSealed
	})
	if err != nil || empty != nil {
		t.Fatalf("unconfigured goalert: transport %v, err %v; want nil, nil", empty, err)
	}

	// A URL without an integration key is a half-finished setup, and saying so
	// beats paging nowhere.
	delete(sealed, notify.SecretGoAlertKey)
	if _, err := notify.LoadGoAlert(ctx, look); err == nil {
		t.Fatal("goalert URL without a key: want an error, got none")
	}
}

// TestBothTransportsSealedBuildTwo is the serve.go wiring in miniature: with
// both ntfy and goalert sealed, loading each and appending the non-nil ones
// hands notify.New two transports, exactly as reactiveEngine does.
func TestBothTransportsSealedBuildTwo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sealed := map[string]string{
		notify.SecretNtfyURL:    "https://push.lan",
		notify.SecretNtfyTopic:  "exocomp",
		notify.SecretGoAlertURL: "https://goalert.lan",
		notify.SecretGoAlertKey: "tok_secret",
	}
	look := func(_ context.Context, name string) ([]byte, error) {
		v, ok := sealed[name]
		if !ok {
			return nil, errNotSealed
		}
		return []byte(v), nil
	}

	var transports []notify.Transport
	ntfyT, err := notify.LoadNtfy(ctx, look)
	if err != nil {
		t.Fatalf("load ntfy: %v", err)
	}
	if ntfyT != nil {
		transports = append(transports, ntfyT)
	}
	goalertT, err := notify.LoadGoAlert(ctx, look)
	if err != nil {
		t.Fatalf("load goalert: %v", err)
	}
	if goalertT != nil {
		transports = append(transports, goalertT)
	}

	n := notify.New(slog.New(slog.DiscardHandler), transports...)
	if n.Transports() != 2 {
		t.Fatalf("notify.New got %d transports, want 2", n.Transports())
	}
}
