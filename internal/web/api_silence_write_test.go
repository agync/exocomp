package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

// bearerDelete issues a DELETE with a bearer token and returns status and body.
func bearerDelete(t *testing.T, base, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, base+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestAPISilenceCreate covers the mute write: unauthenticated is 401, a
// non-operator scope is forbidden (operator-only), and an operator creates a
// silence through the SAME store.CreateSilence the CLI writes.
func TestAPISilenceCreate(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	const path = "/api/v1/silences"
	const body = `{"scope_kind":"host","scope_value":"node-a","reason":"maintenance","for":"2h"}`

	// (a) unauthenticated -> 401
	if code, _, _ := bearerPost(t, ts.URL, path, "", body, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated silence status = %d, want 401", code)
	}

	// (b) wrong scope: readonly may list silences, but not create them.
	ro := h.mint(t, "ro", "readonly")
	code, _, rb := bearerPost(t, ts.URL, path, ro, body, nil)
	if code != http.StatusForbidden {
		t.Fatalf("readonly silence status = %d, want 403 (%s)", code, rb)
	}

	// (c) happy path: an operator mutes a host, and the store holds the row.
	op := h.mint(t, "op", "operator")
	code, _, rb = bearerPost(t, ts.URL, path, op, body, nil)
	if code != http.StatusOK {
		t.Fatalf("operator silence status = %d, want 200 (%s)", code, rb)
	}
	kind, _, _ := decodeEnvelope(t, rb)
	if kind != kindSilence {
		t.Fatalf("silence kind = %q, want %q (%s)", kind, kindSilence, rb)
	}
	var v wire.SilenceView
	decodeData(t, rb, &v)
	if v.ID == 0 || v.ScopeKind != "host" || v.ScopeValue != "node-a" || v.CreatedVia != "api" {
		t.Fatalf("silence view = %+v, want host/node-a via api with an id", v)
	}

	rows, err := h.store.ListSilences(ctx, true)
	if err != nil {
		t.Fatalf("ListSilences: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != v.ID {
		t.Fatalf("store silences = %+v, want the one just created", rows)
	}
}

// TestAPISilenceCreateBadScope proves an unknown scope is a 400, not the 500 the
// store's untyped validation error would map to.
func TestAPISilenceCreateBadScope(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	op := h.mint(t, "op", "operator")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/silences", op,
		`{"scope_kind":"nonsense","for":"2h"}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("bad-scope status = %d, want 400 (%s)", code, body)
	}
}

// TestAPISilenceDelete covers lifting a silence: unauthenticated is 401, a
// non-operator scope is forbidden, an operator lifts an existing silence (204),
// and an unknown id is a 404 — never a silent success.
func TestAPISilenceDelete(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	op := h.mint(t, "op", "operator")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/silences", op,
		`{"scope_kind":"alert","scope_value":"disk-full","for":"1h"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("create for delete status = %d, want 200 (%s)", code, body)
	}
	var v wire.SilenceView
	decodeData(t, body, &v)
	path := "/api/v1/silences/" + itoa64(v.ID)

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
	rows, err := h.store.ListSilences(context.Background(), false)
	if err != nil {
		t.Fatalf("ListSilences: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("store silences = %+v, want empty after delete", rows)
	}

	// A second delete of the now-absent id is a 404.
	if code, rb := bearerDelete(t, ts.URL, path, op); code != http.StatusNotFound {
		t.Fatalf("re-delete status = %d, want 404 (%s)", code, rb)
	}
}
