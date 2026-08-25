package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAPIWhoami covers the identity-reflection endpoint: unauthenticated is 401,
// and any authenticated token — the lowest scope included — gets its own
// identity back. There is no forbidden case: whoami reflects only the caller the
// guard already resolved, so every valid token may read it (canRead), and no
// scope is weakened to make that pass.
func TestAPIWhoami(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// (a) unauthenticated -> 401
	if code, _, _ := bearerGet(t, ts.URL, "/api/v1/whoami", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated whoami status = %d, want 401", code)
	}

	// (c) happy path — the lowest scope (readonly) still resolves, and the
	// reflected identity is the caller's own.
	ro := h.mint(t, "phone", "readonly")
	code, _, body := bearerGet(t, ts.URL, "/api/v1/whoami", ro)
	if code != http.StatusOK {
		t.Fatalf("whoami status = %d, want 200 (%s)", code, body)
	}
	kind, _, _ := decodeEnvelope(t, body)
	if kind != kindWhoami {
		t.Fatalf("whoami kind = %q, want %q (%s)", kind, kindWhoami, body)
	}
	var v whoamiView
	decodeData(t, body, &v)
	if v.Name != "phone" || v.Scope != "readonly" || v.Via != "bearer" {
		t.Fatalf("whoami = %+v, want name=phone scope=readonly via=bearer", v)
	}
	if v.ID == 0 {
		t.Fatalf("whoami has no token id (%s)", body)
	}
}
