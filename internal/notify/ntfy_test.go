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

type publishRecord struct {
	auth string
	body map[string]any
}

func ntfyServer(t *testing.T, status int) (*httptest.Server, *[]publishRecord) {
	t.Helper()
	var seen []publishRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read publish body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode publish body %q: %v", raw, err)
		}
		seen = append(seen, publishRecord{auth: r.Header.Get("Authorization"), body: body})
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":"abc"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestNtfyPublishesTitleActionsAndPriority(t *testing.T) {
	t.Parallel()
	srv, seen := ntfyServer(t, http.StatusOK)

	transport, err := notify.NewNtfy(notify.NtfyOptions{
		BaseURL: srv.URL, Topic: "exocomp", Token: "tk_secret", Client: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new ntfy: %v", err)
	}

	err = notify.New(slog.New(slog.DiscardHandler), transport).Notify(context.Background(),
		notify.Notification{
			Title:    "1 approval waiting",
			Body:     "open to see what it wants to do",
			Priority: notify.PriorityHigh,
			Tags:     []string{"warning"},
			Actions: []notify.Action{
				{Label: "Approve", URL: "http://127.0.0.1:1/approval/1/approve?t=x"},
				{Label: "Reject", URL: "http://127.0.0.1:1/approval/1/reject?t=x"},
			},
		})
	if err != nil {
		t.Fatalf("notify: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("server saw %d publishes, want 1", len(*seen))
	}
	got := (*seen)[0]
	if got.auth != "Bearer tk_secret" {
		t.Fatalf("Authorization = %q, want a bearer token", got.auth)
	}
	if got.body["topic"] != "exocomp" || got.body["title"] != "1 approval waiting" {
		t.Fatalf("published body = %v", got.body)
	}
	if got.body["priority"] != float64(notify.PriorityHigh) {
		t.Fatalf("priority = %v, want %d", got.body["priority"], notify.PriorityHigh)
	}
	actions, ok := got.body["actions"].([]any)
	if !ok || len(actions) != 2 {
		t.Fatalf("actions = %v, want two buttons", got.body["actions"])
	}
	first, _ := actions[0].(map[string]any)
	if first["label"] != "Approve" || first["action"] != "view" {
		t.Fatalf("first action = %v", first)
	}
}

func TestNtfyReportsAServerRefusal(t *testing.T) {
	t.Parallel()
	srv, _ := ntfyServer(t, http.StatusForbidden)

	transport, err := notify.NewNtfy(notify.NtfyOptions{
		BaseURL: srv.URL, Topic: "exocomp", Client: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new ntfy: %v", err)
	}
	err = transport.Publish(context.Background(), notify.Notification{Body: "hi"})
	if err == nil {
		t.Fatal("publish to a refusing server: want an error, got none")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %v, want the server's status in it", err)
	}
}

// TestNtfyRefusesABarePublicTopic is the policy: an approval URL is a bearer
// capability, and a public ntfy.sh topic is a channel anyone who guesses the
// name can read and publish to.
func TestNtfyRefusesABarePublicTopic(t *testing.T) {
	t.Parallel()

	_, err := notify.NewNtfy(notify.NtfyOptions{BaseURL: "https://ntfy.sh", Topic: "exocomp-7f3a"})
	if err == nil {
		t.Fatal("bare ntfy.sh topic: want a refusal, got none")
	}
	for _, want := range []string{"bearer capabilities", "self-host", notify.SecretNtfyToken} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}

	// The same server with a token is allowed: the policy is about
	// authentication, not about the hostname.
	if _, err := notify.NewNtfy(notify.NtfyOptions{
		BaseURL: "https://ntfy.sh", Topic: "exocomp-7f3a", Token: "tk_secret",
	}); err != nil {
		t.Fatalf("ntfy.sh with an access token: %v", err)
	}
	// A self-hosted server needs no token.
	if _, err := notify.NewNtfy(notify.NtfyOptions{
		BaseURL: "https://push.lan", Topic: "exocomp",
	}); err != nil {
		t.Fatalf("self-hosted ntfy without a token: %v", err)
	}
}

// TestNtfyRefusesPublicHostVariants pins the normalization: a tokenless public
// topic must be refused however the host is spelled. url.Hostname does not
// case-fold or strip a trailing dot, so "NTFY.SH" and "ntfy.sh." both reach the
// public server, and a subdomain does too — none may slip past the check.
func TestNtfyRefusesPublicHostVariants(t *testing.T) {
	t.Parallel()

	for _, host := range []string{
		"https://NTFY.SH",
		"https://ntfy.sh.",
		"https://Ntfy.Sh./",
		"https://www.ntfy.sh",
	} {
		if _, err := notify.NewNtfy(notify.NtfyOptions{BaseURL: host, Topic: "exocomp-7f3a"}); err == nil {
			t.Errorf("%s: tokenless public topic accepted, want a refusal", host)
		}
		if _, err := notify.NewNtfy(notify.NtfyOptions{
			BaseURL: host, Topic: "exocomp-7f3a", Token: "tk_secret",
		}); err != nil {
			t.Errorf("%s with a token: %v", host, err)
		}
	}

	// A look-alike host that only ends in the same letters is not ntfy.sh.
	if _, err := notify.NewNtfy(notify.NtfyOptions{
		BaseURL: "https://notntfy.sh", Topic: "exocomp",
	}); err != nil {
		t.Fatalf("host merely ending in the public name must not be treated as public: %v", err)
	}
}

func TestLoadNtfyFromSealedSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sealed := map[string]string{
		notify.SecretNtfyURL:   "https://push.lan\n",
		notify.SecretNtfyTopic: "exocomp\n",
	}
	look := func(_ context.Context, name string) ([]byte, error) {
		v, ok := sealed[name]
		if !ok {
			return nil, errNotSealed
		}
		return []byte(v), nil
	}

	transport, err := notify.LoadNtfy(ctx, look)
	if err != nil {
		t.Fatalf("load ntfy: %v", err)
	}
	if transport == nil {
		t.Fatal("load ntfy returned no transport")
	}
	if got := transport.Endpoint(); got != "https://push.lan/exocomp" {
		t.Fatalf("endpoint = %q", got)
	}

	// No configuration at all is a working daemon with a silent phone.
	empty, err := notify.LoadNtfy(ctx, func(context.Context, string) ([]byte, error) {
		return nil, errNotSealed
	})
	if err != nil || empty != nil {
		t.Fatalf("unconfigured ntfy: transport %v, err %v; want nil, nil", empty, err)
	}

	// A URL without a topic is a half-finished setup, and saying so beats
	// publishing nowhere.
	delete(sealed, notify.SecretNtfyTopic)
	if _, err := notify.LoadNtfy(ctx, look); err == nil {
		t.Fatal("ntfy URL without a topic: want an error, got none")
	}
}

var errNotSealed = errNotSealedType{}

type errNotSealedType struct{}

func (errNotSealedType) Error() string { return "no such secret" }
