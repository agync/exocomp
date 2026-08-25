package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/wire"
)

// TestAPIMuteCreate covers the notify-mute write: unauthenticated is 401, a
// non-operator scope is forbidden (operator-only), and an operator holds a
// host's notifications through the SAME store.CreateNotifyMute the CLI writes.
// A mute is not a silence: the job still runs, only its announcement is held.
func TestAPIMuteCreate(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	const path = "/api/v1/mutes"
	const body = `{"scope_kind":"host","scope_value":"node-a","reason":"noisy","for":"2h"}`

	// (a) unauthenticated -> 401
	if code, _, _ := bearerPost(t, ts.URL, path, "", body, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mute status = %d, want 401", code)
	}

	// (b) wrong scope: readonly may list mutes, but not create them.
	ro := h.mint(t, "ro", "readonly")
	if code, _, rb := bearerPost(t, ts.URL, path, ro, body, nil); code != http.StatusForbidden {
		t.Fatalf("readonly mute status = %d, want 403 (%s)", code, rb)
	}

	// (c) happy path: an operator mutes a host, and the store holds the row.
	op := h.mint(t, "op", "operator")
	code, _, rb := bearerPost(t, ts.URL, path, op, body, nil)
	if code != http.StatusOK {
		t.Fatalf("operator mute status = %d, want 200 (%s)", code, rb)
	}
	kind, _, _ := decodeEnvelope(t, rb)
	if kind != kindMute {
		t.Fatalf("mute kind = %q, want %q (%s)", kind, kindMute, rb)
	}
	var v wire.MuteView
	decodeData(t, rb, &v)
	if v.ID == 0 || v.ScopeKind != "host" || v.ScopeValue != "node-a" || v.CreatedVia != "api" {
		t.Fatalf("mute view = %+v, want host/node-a via api with an id", v)
	}
	if v.ExpiresAt == "" {
		t.Fatalf("mute view has no expires_at (%s)", rb)
	}

	rows, err := h.store.ListNotifyMutes(ctx, true)
	if err != nil {
		t.Fatalf("ListNotifyMutes: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != v.ID {
		t.Fatalf("store mutes = %+v, want the one just created", rows)
	}
}

// TestAPIMuteCreateExpiresAt proves the alternative absolute-expiry form: a
// global mute set with expires_at rather than a `for` duration.
func TestAPIMuteCreateExpiresAt(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	op := h.mint(t, "op", "operator")
	expires := h.server.now().Add(90 * time.Minute).Format(time.RFC3339)
	body := `{"scope_kind":"global","expires_at":"` + expires + `"}`
	code, _, rb := bearerPost(t, ts.URL, "/api/v1/mutes", op, body, nil)
	if code != http.StatusOK {
		t.Fatalf("expires_at mute status = %d, want 200 (%s)", code, rb)
	}
	var v wire.MuteView
	decodeData(t, rb, &v)
	if v.ScopeKind != "global" || v.ExpiresAt == "" {
		t.Fatalf("mute view = %+v, want global with an expiry", v)
	}
}

// TestAPIMuteCreateBadInput proves an unknown scope and a missing expiry are
// both 400s, not the 500 the store's untyped validation would map to.
func TestAPIMuteCreateBadInput(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")

	if code, _, b := bearerPost(t, ts.URL, "/api/v1/mutes", op,
		`{"scope_kind":"nonsense","for":"2h"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("bad-scope status = %d, want 400 (%s)", code, b)
	}
	if code, _, b := bearerPost(t, ts.URL, "/api/v1/mutes", op,
		`{"scope_kind":"global"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("no-expiry status = %d, want 400 (%s)", code, b)
	}
}

// TestAPIMuteDelete covers lifting a mute: unauthenticated is 401, a
// non-operator scope is forbidden, an operator lifts an existing mute (204),
// and an unknown id is a 404 — never a silent success.
func TestAPIMuteDelete(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	op := h.mint(t, "op", "operator")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/mutes", op,
		`{"scope_kind":"alert","scope_value":"disk-full","for":"1h"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("create for delete status = %d, want 200 (%s)", code, body)
	}
	var v wire.MuteView
	decodeData(t, body, &v)
	path := "/api/v1/mutes/" + itoa64(v.ID)

	// (a) unauthenticated -> 401
	if code, _ := bearerDelete(t, ts.URL, path, ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated delete status = %d, want 401", code)
	}

	// (b) wrong scope -> 403
	ro := h.mint(t, "ro", "readonly")
	if code, rb := bearerDelete(t, ts.URL, path, ro); code != http.StatusForbidden {
		t.Fatalf("readonly delete status = %d, want 403 (%s)", code, rb)
	}

	// (c) happy path -> 204, and the row is gone.
	if code, rb := bearerDelete(t, ts.URL, path, op); code != http.StatusNoContent {
		t.Fatalf("operator delete status = %d, want 204 (%s)", code, rb)
	}
	rows, err := h.store.ListNotifyMutes(context.Background(), false)
	if err != nil {
		t.Fatalf("ListNotifyMutes: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("store mutes = %+v, want empty after delete", rows)
	}

	// A second delete of the now-absent id is a 404.
	if code, rb := bearerDelete(t, ts.URL, path, op); code != http.StatusNotFound {
		t.Fatalf("re-delete status = %d, want 404 (%s)", code, rb)
	}
}
