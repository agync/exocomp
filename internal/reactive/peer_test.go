package reactive_test

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// normalizedEvent decodes a stored event's normalized_json back into the Event
// shape, which is where the peer stamping lands.
func normalizedEvent(t *testing.T, ev store.Event) reactive.Event {
	t.Helper()
	var norm reactive.Event
	if err := json.Unmarshal([]byte(ev.Normalized), &norm); err != nil {
		t.Fatalf("unmarshal normalized event: %v", err)
	}
	return norm
}

// TestWebhookPeerStampedWithHomeGroup: an httptest peer is always 127.0.0.1, so
// a netgroup covering it must name the group on every event the hook records.
// peer_ip must be present regardless of the group lookup.
func TestWebhookPeerStampedWithHomeGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name:     "localhost",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
	}); err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	srv := h.serve(t, "")
	body := []byte(`{"event": "deploy", "status": "firing"}`)
	url := srv.URL + reactive.HookPrefix + reactive.HookToken(h.deriver, reactive.SourceGeneric)
	code, response := post(t, srv.Client(), url, body, nil)
	if code != 202 {
		t.Fatalf("hook answered %d: %s", code, response)
	}

	norm := normalizedEvent(t, lastEvent(t, h.store))
	if norm.Labels["peer_ip"] != "127.0.0.1" {
		t.Errorf("peer_ip = %q, want 127.0.0.1 (labels %v)", norm.Labels["peer_ip"], norm.Labels)
	}
	if norm.Labels["peer_netgroup"] != "localhost" {
		t.Errorf("peer_netgroup = %q, want localhost (labels %v)", norm.Labels["peer_netgroup"], norm.Labels)
	}
}

// TestWebhookPeerAwayWhenNoGroupCovers: a netgroup that does not contain the
// peer (10.0.0.0/8 never contains 127.0.0.1) must stamp "away", not the
// registered group name. peer_ip is still present: the path is always known,
// only its classification can be missing. If the lookup result were hardcoded
// to a group name, this test would fail.
func TestWebhookPeerAwayWhenNoGroupCovers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, harnessOptions{})
	if _, err := h.store.CreateNetgroup(ctx, store.NewNetgroup{
		Name:     "corp",
		Prefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	}); err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	srv := h.serve(t, "")
	body := []byte(`{"alertname": "nfs-down", "status": "firing"}`)
	url := srv.URL + reactive.HookPrefix + reactive.HookToken(h.deriver, reactive.SourceGeneric)
	code, response := post(t, srv.Client(), url, body, nil)
	if code != 202 {
		t.Fatalf("hook answered %d: %s", code, response)
	}

	norm := normalizedEvent(t, lastEvent(t, h.store))
	if norm.Labels["peer_ip"] != "127.0.0.1" {
		t.Errorf("peer_ip = %q, want 127.0.0.1 (labels %v)", norm.Labels["peer_ip"], norm.Labels)
	}
	if norm.Labels["peer_netgroup"] != "away" {
		t.Errorf("peer_netgroup = %q, want away (labels %v)", norm.Labels["peer_netgroup"], norm.Labels)
	}
	if strings.Contains(norm.Labels["peer_netgroup"], "corp") {
		t.Errorf("a non-covering group leaked into the stamp: %q", norm.Labels["peer_netgroup"])
	}
}
