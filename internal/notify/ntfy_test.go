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

// TestNtfyDescriptiveDetailNamesTheConditionNotTheSecrets is the default
// (Detail unset, i.e. DetailDescriptive): the push carries the notification's
// own non-sensitive identifier and its deep link, the way every builder in
// internal/reactive already composes one — but the transport's own secrets,
// the relay bearer token and the capability token folded into the deep link,
// must appear only where they belong (the Authorization header, the action
// URL) and never get duplicated into the human-readable title or body.
func TestNtfyDescriptiveDetailNamesTheConditionNotTheSecrets(t *testing.T) {
	t.Parallel()
	srv, seen := ntfyServer(t, http.StatusOK)

	const relayToken = "relay_bearer_secret"
	const capToken = "approval_capability_secret"

	transport, err := notify.NewNtfy(notify.NtfyOptions{
		BaseURL: srv.URL, Topic: "exocomp", Token: relayToken, Client: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new ntfy: %v", err)
	}

	deepLink := "https://exocomp.lan/approval/7/approve?t=" + capToken
	err = transport.Publish(context.Background(), notify.Notification{
		Title:    "job failed: nightly-backup",
		Body:     "Job 7 (tofu) ended failed.",
		Priority: notify.PriorityHigh,
		Actions:  []notify.Action{{Label: "Open", URL: deepLink}},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("server saw %d publishes, want 1", len(*seen))
	}
	got := (*seen)[0]
	if got.body["title"] != "job failed: nightly-backup" {
		t.Fatalf("title = %v, want the notification's own identifier", got.body["title"])
	}
	if got.body["message"] != "Job 7 (tofu) ended failed." {
		t.Fatalf("message = %v, want the notification's own summary", got.body["message"])
	}
	actions, ok := got.body["actions"].([]any)
	if !ok || len(actions) != 1 {
		t.Fatalf("actions = %v, want the one deep link", got.body["actions"])
	}
	first, _ := actions[0].(map[string]any)
	if first["url"] != deepLink {
		t.Fatalf("action url = %v, want the deep link", first["url"])
	}

	raw, _ := json.Marshal(got.body)
	published := string(raw)
	if strings.Contains(published, relayToken) {
		t.Fatalf("published body leaks the relay bearer token: %s", published)
	}
	if got.auth != "Bearer "+relayToken {
		t.Fatalf("Authorization = %q, want the bearer token there instead", got.auth)
	}
	if strings.Count(published, capToken) != 1 {
		t.Fatalf("capability token should appear exactly once, in the action url: %s", published)
	}
}

// TestNtfyTerseDetailDropsTheSummary pins DetailTerse: whatever the
// notification names, the push is the fixed generic phrase and nothing else —
// only the deep link survives, for an operator who does not trust their relay
// with any identifying text.
func TestNtfyTerseDetailDropsTheSummary(t *testing.T) {
	t.Parallel()
	srv, seen := ntfyServer(t, http.StatusOK)

	transport, err := notify.NewNtfy(notify.NtfyOptions{
		BaseURL: srv.URL, Topic: "exocomp", Client: srv.Client(), Detail: notify.DetailTerse,
	})
	if err != nil {
		t.Fatalf("new ntfy: %v", err)
	}

	deepLink := "https://exocomp.lan/approval/7/approve?t=tok"
	err = transport.Publish(context.Background(), notify.Notification{
		Title:    "job failed: nightly-backup",
		Body:     "Job 7 (tofu) ended failed. Last error: apply exited 1",
		Priority: notify.PriorityHigh,
		Tags:     []string{"rotating_light"},
		Actions:  []notify.Action{{Label: "Open", URL: deepLink}},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("server saw %d publishes, want 1", len(*seen))
	}
	got := (*seen)[0]
	if got.body["title"] != "exocomp" {
		t.Fatalf("title = %v, want the fixed generic title", got.body["title"])
	}
	if got.body["message"] != "exocomp: action required" {
		t.Fatalf("message = %v, want the fixed generic body", got.body["message"])
	}
	if got.body["priority"] != float64(notify.PriorityHigh) {
		t.Fatalf("priority = %v, want it preserved so the phone still buzzes right", got.body["priority"])
	}
	actions, ok := got.body["actions"].([]any)
	if !ok || len(actions) != 1 {
		t.Fatalf("actions = %v, want the deep link preserved", got.body["actions"])
	}
	first, _ := actions[0].(map[string]any)
	if first["url"] != deepLink {
		t.Fatalf("action url = %v, want the deep link preserved", first["url"])
	}

	raw, _ := json.Marshal(got.body)
	published := string(raw)
	for _, leaked := range []string{"nightly-backup", "Job 7", "apply exited 1"} {
		if strings.Contains(published, leaked) {
			t.Fatalf("terse push leaks %q: %s", leaked, published)
		}
	}
}

// TestNtfySetDetailOverridesAfterConstruction is the LoadNtfy path: the
// transport is built from sealed secrets alone, and the operator's Detail
// setting is applied afterward.
func TestNtfySetDetailOverridesAfterConstruction(t *testing.T) {
	t.Parallel()
	srv, seen := ntfyServer(t, http.StatusOK)

	transport, err := notify.NewNtfy(notify.NtfyOptions{
		BaseURL: srv.URL, Topic: "exocomp", Client: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new ntfy: %v", err)
	}
	transport.SetDetail(notify.DetailTerse)

	if err := transport.Publish(context.Background(), notify.Notification{Title: "x", Body: "y"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if (*seen)[0].body["title"] != "exocomp" {
		t.Fatalf("title = %v, want SetDetail's terse override applied", (*seen)[0].body["title"])
	}

	// An unrecognized value (a stale build, a typo) falls back to the
	// documented default rather than silently keeping whatever was set before.
	transport.SetDetail(notify.Detail("bogus"))
	if err := transport.Publish(context.Background(), notify.Notification{Title: "x", Body: "y"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if (*seen)[1].body["title"] != "x" {
		t.Fatalf("title = %v, want an unrecognized Detail to fall back to descriptive", (*seen)[1].body["title"])
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
