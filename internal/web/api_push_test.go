package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/wire"
)

// a public, non-private, non-loopback literal so the happy path validates
// without any real DNS lookup in the test (203.0.113.0/24 is TEST-NET-3).
const validPushEndpoint = "https://203.0.113.10/up/abc"

// TestAPIPushEndpointRegister covers the register write: unauthenticated is 401,
// readonly/approve are forbidden (operator-only), and an operator registers an
// endpoint and gets the values-free view back — no token material echoed.
func TestAPIPushEndpointRegister(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	const path = "/api/v1/notify/endpoints"
	body := `{"endpoint":"` + validPushEndpoint + `","label":"pixel"}`

	if code, _, _ := bearerPost(t, ts.URL, path, "", body, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated register = %d, want 401", code)
	}
	for _, scope := range []string{"readonly", "approve"} {
		tok := h.mint(t, scope, scope)
		if code, _, rb := bearerPost(t, ts.URL, path, tok, body, nil); code != http.StatusForbidden {
			t.Fatalf("%s register = %d, want 403 (%s)", scope, code, rb)
		}
	}

	op := h.mint(t, "op", "operator")
	code, _, rb := bearerPost(t, ts.URL, path, op, body, nil)
	if code != http.StatusOK {
		t.Fatalf("operator register = %d, want 200 (%s)", code, rb)
	}
	kind, _, _ := decodeEnvelope(t, rb)
	if kind != wire.KindPushEndpoint {
		t.Fatalf("register kind = %q, want %q", kind, wire.KindPushEndpoint)
	}
	var v wire.PushEndpointView
	decodeData(t, rb, &v)
	if v.ID == 0 || v.Endpoint != validPushEndpoint || v.Label != "pixel" || v.CreatedAt == "" {
		t.Fatalf("register view = %+v, want the endpoint with an id/label/created_at", v)
	}
	// No token identity is echoed in the response body.
	for _, banned := range []string{"token", "\"op\"", "token_id"} {
		if strings.Contains(rb, banned) {
			t.Fatalf("register response leaked %q: %s", banned, rb)
		}
	}

	rows, err := h.store.ListPushEndpoints(ctx)
	if err != nil {
		t.Fatalf("ListPushEndpoints: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != v.ID {
		t.Fatalf("store endpoints = %+v, want the one just created", rows)
	}
}

// TestAPIPushEndpointSSRFRefused is the security-critical assertion: a
// registration whose URL is non-https, or whose host is loopback / a private
// address / the cloud metadata IP, is refused with a 400 before any row is
// stored. Each of these is caught at registration without needing DNS, because
// each host is a literal.
func TestAPIPushEndpointSSRFRefused(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")

	refused := []string{
		"http://up.example.com/x",   // not https
		"https://127.0.0.1/x",       // loopback
		"https://10.0.0.5/x",        // RFC1918 private
		"https://[::1]/x",           // ipv6 loopback
		"https://169.254.169.254/x", // cloud metadata
	}
	for _, ep := range refused {
		body := `{"endpoint":"` + ep + `"}`
		code, _, rb := bearerPost(t, ts.URL, "/api/v1/notify/endpoints", op, body, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("register %s = %d, want 400 refusal (%s)", ep, code, rb)
		}
		_, errCode, _ := decodeEnvelope(t, rb)
		if errCode != codeInvalidArgument {
			t.Fatalf("register %s error code = %q, want %q", ep, errCode, codeInvalidArgument)
		}
	}

	// None of the refused registrations left a row behind.
	rows, err := h.store.ListPushEndpoints(context.Background())
	if err != nil {
		t.Fatalf("ListPushEndpoints: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("store endpoints = %+v, want none after every refusal", rows)
	}
}

// TestAPIPushEndpointListScoped proves a token lists only its own endpoints:
// two operator tokens each register, and each GET returns only that token's row.
func TestAPIPushEndpointListScoped(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	const path = "/api/v1/notify/endpoints"

	opA := h.mint(t, "op-a", "operator")
	opB := h.mint(t, "op-b", "operator")
	if code, _, rb := bearerPost(t, ts.URL, path, opA, `{"endpoint":"`+validPushEndpoint+`"}`, nil); code != http.StatusOK {
		t.Fatalf("op-a register = %d (%s)", code, rb)
	}

	code, _, rb := bearerGet(t, ts.URL, path, opA)
	if code != http.StatusOK {
		t.Fatalf("op-a list = %d (%s)", code, rb)
	}
	var mine []wire.PushEndpointView
	decodeData(t, rb, &mine)
	if len(mine) != 1 {
		t.Fatalf("op-a list = %d rows, want 1", len(mine))
	}

	code, _, rb = bearerGet(t, ts.URL, path, opB)
	if code != http.StatusOK {
		t.Fatalf("op-b list = %d (%s)", code, rb)
	}
	var theirs []wire.PushEndpointView
	decodeData(t, rb, &theirs)
	if len(theirs) != 0 {
		t.Fatalf("op-b list = %d rows, want 0 (must not see op-a's)", len(theirs))
	}
}

// TestAPIPushEndpointDeleteScoped proves delete is scoped to the owner: an
// unknown id is 404, another token's id is 404 (never a cross-device drop), and
// the owner deletes its own with a 204.
func TestAPIPushEndpointDeleteScoped(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	const path = "/api/v1/notify/endpoints"

	opA := h.mint(t, "op-a", "operator")
	opB := h.mint(t, "op-b", "operator")
	_, _, rb := bearerPost(t, ts.URL, path, opA, `{"endpoint":"`+validPushEndpoint+`"}`, nil)
	var v wire.PushEndpointView
	decodeData(t, rb, &v)
	epPath := path + "/" + itoa64(v.ID)

	// Unknown id -> 404.
	if code, _ := bearerDelete(t, ts.URL, path+"/999999", opA); code != http.StatusNotFound {
		t.Fatalf("delete unknown = %d, want 404", code)
	}
	// A different owner may not delete it (and it reads as not-found).
	if code, _ := bearerDelete(t, ts.URL, epPath, opB); code != http.StatusNotFound {
		t.Fatalf("op-b delete op-a's = %d, want 404", code)
	}
	// The owner deletes its own.
	if code, body := bearerDelete(t, ts.URL, epPath, opA); code != http.StatusNoContent {
		t.Fatalf("op-a delete own = %d, want 204 (%s)", code, body)
	}
	// The endpoint really is gone; op-b's earlier probe never removed it.
	rows, err := h.store.ListPushEndpoints(context.Background())
	if err != nil {
		t.Fatalf("ListPushEndpoints: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("store endpoints = %+v, want empty after owner delete", rows)
	}
}

// TestAPIPushEndpointCap proves a token cannot register past its endpoint cap.
func TestAPIPushEndpointCap(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	const path = "/api/v1/notify/endpoints"
	op := h.mint(t, "op", "operator")

	for i := 0; i < maxPushEndpointsPerToken; i++ {
		ep := `{"endpoint":"https://203.0.113.10/up/` + itoa64(int64(i)) + `"}`
		if code, _, rb := bearerPost(t, ts.URL, path, op, ep, nil); code != http.StatusOK {
			t.Fatalf("register %d = %d, want 200 (%s)", i, code, rb)
		}
	}
	code, _, rb := bearerPost(t, ts.URL, path, op, `{"endpoint":"https://203.0.113.10/up/over"}`, nil)
	if code != http.StatusConflict {
		t.Fatalf("over-cap register = %d, want 409 (%s)", code, rb)
	}
}
