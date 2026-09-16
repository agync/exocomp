package web

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// bearerPut issues a PUT with a bearer token and a JSON body, returning status
// and body.
func bearerPut(t *testing.T, base, path, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestAPICheckWriteRoundTrip drives create/update/enable/disable/delete over
// /api/v1 and proves each routes through the same store verbs the CLI uses.
func TestAPICheckWriteRoundTrip(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	ctx := context.Background()

	code, _, body := bearerPost(t, ts.URL, "/api/v1/checks", token,
		`{"name":"at-home","kind":"tcp","interval":"5m","targets":["laptop=192.168.1.50:22"]}`, nil)
	if code != http.StatusOK {
		t.Fatalf("create status = %d, want 200 (%s)", code, body)
	}
	if kind, _, _ := decodeEnvelope(t, body); kind != wire.KindCheck {
		t.Fatalf("create kind = %q, want %q (%s)", kind, wire.KindCheck, body)
	}
	var created wire.CheckView
	decodeData(t, body, &created)
	if created.Name != "at-home" || created.IntervalS != 300 || len(created.Targets) != 1 {
		t.Fatalf("created check = %+v", created)
	}
	if got, err := h.store.GetCheck(ctx, "at-home"); err != nil || got.ID != created.ID {
		t.Fatalf("create did not persist: %+v (%v)", got, err)
	}

	// The read route sees it.
	code, _, body = bearerGet(t, ts.URL, "/api/v1/checks", token)
	if code != http.StatusOK {
		t.Fatalf("GET checks status = %d (%s)", code, body)
	}
	var list []wire.CheckView
	if kind, _, data := decodeEnvelope(t, body); kind != wire.KindCheckList {
		t.Fatalf("list kind = %q (%s)", kind, body)
	} else if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].Name != "at-home" {
		t.Fatalf("list = %+v", list)
	}

	code, body = bearerPut(t, ts.URL, "/api/v1/checks/at-home", token,
		`{"interval":"10m","targets":["laptop=192.168.1.51:22"]}`)
	if code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (%s)", code, body)
	}
	updated, err := h.store.GetCheck(ctx, "at-home")
	if err != nil {
		t.Fatalf("GetCheck: %v", err)
	}
	if updated.IntervalS != 600 || updated.Targets[0].Address != "192.168.1.51" {
		t.Fatalf("updated check = %+v", updated)
	}

	code, _, body = bearerPost(t, ts.URL, "/api/v1/checks/at-home/disable", token, "", nil)
	if code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200 (%s)", code, body)
	}
	if got, _ := h.store.GetCheck(ctx, "at-home"); got.Enabled {
		t.Fatal("disable left the check enabled")
	}
	code, _, _ = bearerPost(t, ts.URL, "/api/v1/checks/at-home/enable", token, "", nil)
	if code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200", code)
	}
	if got, _ := h.store.GetCheck(ctx, "at-home"); !got.Enabled {
		t.Fatal("enable left the check disabled")
	}

	code, body = bearerDelete(t, ts.URL, "/api/v1/checks/at-home", token)
	if code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (%s)", code, body)
	}
	if _, err := h.store.GetCheck(ctx, "at-home"); err == nil {
		t.Fatal("delete left the row in the store")
	}
}

// TestAPICheckWriteScopeGate asserts the check write routes take the operator
// bearer floor: readonly is forbidden before any store touch, no token is 401.
func TestAPICheckWriteScopeGate(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	const body = `{"name":"ghost","kind":"tcp","interval":"5m","targets":["a=192.168.1.1:22"]}`

	ro := h.mint(t, "ro", "readonly")
	code, _, rb := bearerPost(t, ts.URL, "/api/v1/checks", ro, body, nil)
	if code != http.StatusForbidden {
		t.Fatalf("readonly create status = %d, want 403 (%s)", code, rb)
	}
	if kind, ecode, _ := decodeEnvelope(t, rb); kind != "error" || ecode != "forbidden" {
		t.Fatalf("readonly create envelope = %s", rb)
	}
	if _, err := h.store.GetCheck(ctx, "ghost"); err == nil {
		t.Fatal("a refused create still wrote the row")
	}

	code, _, rb = bearerPost(t, ts.URL, "/api/v1/checks", "", body, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token create status = %d, want 401 (%s)", code, rb)
	}
	if kind, ecode, _ := decodeEnvelope(t, rb); kind != "error" || ecode != "unauthenticated" {
		t.Fatalf("no-token create envelope = %s", rb)
	}
}

// TestAPICheckProbeReadScope asserts the probe route is read-scope (D3): a
// readonly token gets the real per-target verdict, and nothing is written.
func TestAPICheckProbeReadScope(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	c, err := h.store.CreateCheck(ctx, store.NewCheck{
		Name: "at-home", Kind: store.CheckTCP, IntervalS: 300,
		Targets: []store.CheckTarget{
			{Name: "up", Address: "127.0.0.1", Port: port},
			{Name: "down", Address: "127.0.0.1", Port: 1},
		},
	})
	if err != nil {
		t.Fatalf("CreateCheck: %v", err)
	}
	before, _ := h.store.GetCheck(ctx, "at-home")

	ro := h.mint(t, "ro", "readonly")
	code, _, body := bearerPost(t, ts.URL, "/api/v1/checks/at-home/probe", ro, "", nil)
	if code != http.StatusOK {
		t.Fatalf("probe status = %d, want 200 for readonly (%s)", code, body)
	}
	if kind, _, _ := decodeEnvelope(t, body); kind != wire.KindCheckProbe {
		t.Fatalf("probe kind = %q (%s)", kind, body)
	}
	var view wire.CheckProbeView
	decodeData(t, body, &view)
	if view.Check != "at-home" || len(view.Targets) != 2 {
		t.Fatalf("probe view = %+v", view)
	}
	byName := map[string]wire.CheckProbeTargetView{}
	for _, tgt := range view.Targets {
		byName[tgt.Target] = tgt
	}
	if !byName["up"].Up || byName["down"].Up {
		t.Fatalf("probe verdicts = %+v", byName)
	}

	if states, err := h.store.ListCheckStates(ctx, c.ID); err != nil || len(states) != 0 {
		t.Fatalf("probe wrote check state: %v (%v)", states, err)
	}
	after, _ := h.store.GetCheck(ctx, "at-home")
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("probe touched the check row")
	}

	// unknown target is a boundary error.
	code, _, body = bearerPost(t, ts.URL, "/api/v1/checks/at-home/probe?target=ghost", ro, "", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown target status = %d, want 400 (%s)", code, body)
	}
	if kind, ecode, _ := decodeEnvelope(t, body); kind != "error" || ecode != codeInvalidArgument {
		t.Fatalf("unknown target envelope = %s", body)
	}
}

// TestAPINetgroupWriteRoundTrip drives netgroup create/update/delete over
// /api/v1 and proves the CIDR set persists with exclusions.
func TestAPINetgroupWriteRoundTrip(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")
	ctx := context.Background()

	code, _, body := bearerPost(t, ts.URL, "/api/v1/netgroups", token,
		`{"name":"home","prefixes":"192.168.0.0/16,!192.168.5.0/24"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("create status = %d, want 200 (%s)", code, body)
	}
	if kind, _, _ := decodeEnvelope(t, body); kind != wire.KindNetgroup {
		t.Fatalf("create kind = %q (%s)", kind, body)
	}
	var view wire.NetgroupView
	decodeData(t, body, &view)
	if view.Name != "home" || len(view.Prefixes) != 1 || len(view.Excludes) != 1 {
		t.Fatalf("netgroup view = %+v", view)
	}
	if got, err := h.store.GetNetgroup(ctx, "home"); err != nil || len(got.Excludes) != 1 {
		t.Fatalf("create did not persist: %+v (%v)", got, err)
	}

	code, body = bearerPut(t, ts.URL, "/api/v1/netgroups/home", token,
		`{"prefixes":"10.0.0.0/8"}`)
	if code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (%s)", code, body)
	}
	got, err := h.store.GetNetgroup(ctx, "home")
	if err != nil {
		t.Fatalf("GetNetgroup: %v", err)
	}
	if len(got.Prefixes) != 1 || got.Prefixes[0].String() != "10.0.0.0/8" || len(got.Excludes) != 0 {
		t.Fatalf("updated netgroup = %+v", got)
	}

	code, body = bearerDelete(t, ts.URL, "/api/v1/netgroups/home", token)
	if code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (%s)", code, body)
	}
	if _, err := h.store.GetNetgroup(ctx, "home"); err == nil {
		t.Fatal("delete left the row in the store")
	}
}

// TestAPICheckWriteBadBody asserts a malformed target or missing name is a 400
// invalid_argument, and a duplicate name is the store's 409 duplicate.
func TestAPICheckWriteBadBody(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	for name, body := range map[string]string{
		"missing name": `{"kind":"tcp","interval":"5m","targets":["a=192.168.1.1:22"]}`,
		"bad target":   `{"name":"x","kind":"tcp","interval":"5m","targets":["nope"]}`,
		"bad interval": `{"name":"x","kind":"tcp","interval":"soon","targets":["a=192.168.1.1:22"]}`,
	} {
		code, _, rb := bearerPost(t, ts.URL, "/api/v1/checks", token, body, nil)
		if code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400 (%s)", name, code, rb)
			continue
		}
		if kind, ecode, _ := decodeEnvelope(t, rb); kind != "error" || ecode != codeInvalidArgument {
			t.Errorf("%s envelope = %s", name, rb)
		}
	}

	body := `{"name":"dup","kind":"tcp","interval":"5m","targets":["a=192.168.1.1:22"]}`
	if code, _, rb := bearerPost(t, ts.URL, "/api/v1/checks", token, body, nil); code != http.StatusOK {
		t.Fatalf("first create status = %d (%s)", code, rb)
	}
	code, _, rb := bearerPost(t, ts.URL, "/api/v1/checks", token, body, nil)
	if code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409 (%s)", code, rb)
	}
	if kind, ecode, _ := decodeEnvelope(t, rb); kind != "error" || ecode != wire.CodeDuplicate {
		t.Fatalf("duplicate envelope = %s", rb)
	}

	// A missing check on update is a 404, not a silent success.
	code, rb = bearerPut(t, ts.URL, "/api/v1/checks/ghost", token, `{"targets":["a=192.168.1.1:22"]}`)
	if code != http.StatusNotFound {
		t.Fatalf("update unknown status = %d, want 404 (%s)", code, rb)
	}
}
