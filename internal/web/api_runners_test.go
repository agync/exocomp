package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// TestAPIRunnersEmptyEnvelope: an empty store still answers 200 with a wire
// success envelope carrying "data":[], matching every other list route (never
// a 404 for an empty collection).
func TestAPIRunnersEmptyEnvelope(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	code, hdr, body := bearerGet(t, ts.URL, "/api/v1/runners", token)
	if code != 200 {
		t.Fatalf("GET /api/v1/runners status = %d, want 200 (%s)", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var env struct {
		SchemaVersion int             `json:"schema_version"`
		Kind          string          `json:"kind"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	if env.SchemaVersion != wire.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", env.SchemaVersion, wire.SchemaVersion)
	}
	if env.Kind != wire.KindRunnerList {
		t.Errorf("kind = %q, want %q", env.Kind, wire.KindRunnerList)
	}
	if strings.TrimSpace(string(env.Data)) != "[]" {
		t.Errorf("empty-store data = %s, want []", env.Data)
	}
}

// TestAPIRunnersNoTokenIsWireError: no token gets 401 and a wire error
// envelope on both the list and single-object route.
func TestAPIRunnersNoTokenIsWireError(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	for _, p := range []string{"/api/v1/runners", "/api/v1/runners/fedora-ee"} {
		code, hdr, body := bearerGet(t, ts.URL, p, "")
		if code != 401 {
			t.Errorf("GET %s (no token) status = %d, want 401", p, code)
		}
		if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s (no token) content-type = %q, want application/json", p, ct)
		}
		var env struct {
			Kind  string     `json:"kind"`
			Error wire.Error `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("GET %s (no token) body is not a wire envelope: %v (%s)", p, err, body)
		}
		if env.Kind != wire.KindError || env.Error.Code != wire.CodeUnauthenticated {
			t.Errorf("GET %s (no token) = %s, want error/unauthenticated", p, body)
		}
	}
}

// TestAPIRunnerGetNotFound: an unknown runner name is 404 with not_found;
// blank name (the route's own {name} segment being empty) never routes here
// on the standard mux, so only the not-found case is exercised.
func TestAPIRunnerGetNotFound(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	code, _, body := bearerGet(t, ts.URL, "/api/v1/runners/does-not-exist", token)
	if code != 404 {
		t.Fatalf("missing runner status = %d, want 404 (%s)", code, body)
	}
	var env struct {
		Kind  string     `json:"kind"`
		Error wire.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("404 body is not a wire envelope: %v (%s)", err, body)
	}
	if env.Kind != wire.KindError || env.Error.Code != wire.CodeNotFound {
		t.Fatalf("404 envelope = %s, want error/not_found", body)
	}
}

// TestAPIRunnersBytesMatchCLI proves the API body is byte-identical to
// wire.MarshalSuccess over the same store rows for both the list and the
// single-object route — the shared projection the CLI's `--output json` uses.
func TestAPIRunnersBytesMatchCLI(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")
	ctx := context.Background()

	if _, err := h.store.CreateRunner(ctx, store.Runner{
		Name:        "fedora-ee",
		Mode:        store.RunnerModeCached,
		Base:        "registry.fedoraproject.org/fedora-minimal:44",
		Bootstrap:   "microdnf install -y sshpass rsync\n",
		Packages:    []string{"rsync", "sshpass"},
		AnsibleCore: "2.18.1",
		MemoryMax:   "2G",
		CPUQuota:    "200%",
	}); err != nil {
		t.Fatalf("seed fedora-ee: %v", err)
	}
	if _, err := h.store.CreateRunner(ctx, store.Runner{
		Name:          "dev-shell",
		Mode:          store.RunnerModeOnTheFly,
		Base:          "registry.fedoraproject.org/fedora-minimal:44",
		Containerfile: ".exocomp/Containerfile",
	}); err != nil {
		t.Fatalf("seed dev-shell: %v", err)
	}

	rows, err := h.store.ListRunners(ctx)
	if err != nil {
		t.Fatalf("ListRunners: %v", err)
	}
	want, err := wire.MarshalSuccess(wire.KindRunnerList, wire.NewRunnerList(rows))
	if err != nil {
		t.Fatalf("MarshalSuccess: %v", err)
	}
	_, _, body := bearerGet(t, ts.URL, "/api/v1/runners", token)
	if body != string(want) {
		t.Fatalf("list bytes differ from CLI projection:\n api: %q\n cli: %q", body, want)
	}

	rn := rows[0]
	want, err = wire.MarshalSuccess(wire.KindRunner, wire.NewRunnerView(rn))
	if err != nil {
		t.Fatalf("MarshalSuccess get: %v", err)
	}
	_, _, body = bearerGet(t, ts.URL, "/api/v1/runners/"+rn.Name, token)
	if body != string(want) {
		t.Fatalf("get bytes differ from CLI projection:\n api: %q\n cli: %q", body, want)
	}

	// The bootstrap script body never appears in the wire bytes: the view has
	// no field for it, only its digest and line count.
	if strings.Contains(body, "microdnf install") {
		t.Error("runner API response leaked the bootstrap script body")
	}
}
