package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/metrics"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// bearerGet issues a GET with a bearer token and returns status, headers, body.
func bearerGet(t *testing.T, base, path, token string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

// apiV1Paths is every read route this file mounts. The list ones answer on an
// empty store; that is the point — an empty collection must still be a 200 with
// a wire envelope, not a 404.
var apiV1Paths = []string{
	"/api/v1/jobs",
	"/api/v1/workspaces",
	"/api/v1/sources",
	"/api/v1/approvals",
	"/api/v1/schedules",
	"/api/v1/netgroups",
	"/api/v1/rules",
	"/api/v1/silences",
	"/api/v1/mutes",
	"/api/v1/events",
	"/api/v1/secrets",
	"/api/v1/backups",
	"/api/v1/checks",
}

// TestAPIReadonlyGetsWireEnvelope: a readonly token gets 200 and a well-formed
// wire success envelope (schema_version, a kind, a data field) on every list
// route. This is the primary "the machine face answers" gate.
func TestAPIReadonlyGetsWireEnvelope(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	for _, p := range apiV1Paths {
		code, hdr, body := bearerGet(t, ts.URL, p, token)
		if code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200 (body: %s)", p, code, body)
			continue
		}
		if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s content-type = %q, want application/json", p, ct)
		}
		var env struct {
			SchemaVersion int             `json:"schema_version"`
			Kind          string          `json:"kind"`
			Data          json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Errorf("GET %s body is not JSON: %v (%s)", p, err, body)
			continue
		}
		if env.SchemaVersion != wire.SchemaVersion {
			t.Errorf("GET %s schema_version = %d, want %d", p, env.SchemaVersion, wire.SchemaVersion)
		}
		if env.Kind == "" || env.Kind == wire.KindError {
			t.Errorf("GET %s kind = %q, want a success kind", p, env.Kind)
		}
		// An empty list renders "data":[], never null or absent.
		if strings.TrimSpace(string(env.Data)) != "[]" {
			t.Errorf("GET %s empty-store data = %s, want []", p, env.Data)
		}
	}
}

// TestAPINoTokenIsWireError: no token gets 401 AND a wire error envelope — not
// HTML, not the browser's bare "unauthorized" string, not a 303 to /login.
func TestAPINoTokenIsWireError(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	for _, p := range apiV1Paths {
		code, hdr, body := bearerGet(t, ts.URL, p, "")
		if code != http.StatusUnauthorized {
			t.Errorf("GET %s (no token) status = %d, want 401", p, code)
		}
		if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s (no token) content-type = %q, want application/json", p, ct)
		}
		var env struct {
			SchemaVersion int         `json:"schema_version"`
			Kind          string      `json:"kind"`
			Error         *wire.Error `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Errorf("GET %s (no token) body is not a wire envelope: %v (%s)", p, err, body)
			continue
		}
		if env.Kind != wire.KindError || env.Error == nil {
			t.Errorf("GET %s (no token) is not an error envelope: %s", p, body)
			continue
		}
		if env.Error.Code != wire.CodeUnauthenticated {
			t.Errorf("GET %s (no token) code = %q, want %q", p, env.Error.Code, wire.CodeUnauthenticated)
		}
	}
}

// TestAPIWriteMethodIs405: a write HTTP method on an /api/v1 read path is
// refused by the mux's method-aware routing. No mutating route exists in this
// file, so every non-GET is a 405.
func TestAPIWriteMethodIs405(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(method, ts.URL+"/api/v1/jobs", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s /api/v1/jobs: %v", method, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/jobs status = %d, want 405", method, resp.StatusCode)
		}
	}
}

// TestAPIJobNotFound: a well-formed but unknown job id is a 404 carrying the
// not_found wire error code, while a live job is a 200 with the job.get kind.
func TestAPIJobNotFound(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	code, _, body := bearerGet(t, ts.URL, "/api/v1/jobs/999999", token)
	if code != http.StatusNotFound {
		t.Fatalf("missing job status = %d, want 404 (%s)", code, body)
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

	// A non-numeric id is a 400 (a malformed URL, not a missing row).
	if code, _, _ := bearerGet(t, ts.URL, "/api/v1/jobs/not-a-number", token); code != http.StatusBadRequest {
		t.Fatalf("non-numeric id status = %d, want 400", code)
	}

	// A real job answers 200 with the single-object kind.
	job, err := h.queue.Enqueue(context.Background(), queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	code, _, body = bearerGet(t, ts.URL, "/api/v1/jobs/"+itoa64(job.ID), token)
	if code != http.StatusOK {
		t.Fatalf("job get status = %d, want 200 (%s)", code, body)
	}
	if !strings.Contains(body, `"kind":"`+wire.KindJob+`"`) {
		t.Fatalf("job get kind missing: %s", body)
	}
}

// TestAPIJobBytesMatchCLI proves the API body is byte-identical to what the CLI
// emits for the same resource: both go through the shared wire projection and
// marshaller. Asserting against wire.MarshalSuccess over the same store rows IS
// asserting the shared projection the CLI's `--output json` uses.
func TestAPIJobBytesMatchCLI(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")
	ctx := context.Background()

	// Enqueue through the queue and wait for each to settle, so the harness's
	// live workers are done mutating the rows before we snapshot them — the same
	// de-race the other post-v1 seed helpers use.
	for i := 0; i < 3; i++ {
		job, err := h.queue.Enqueue(ctx, queue.Spec{
			Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		h.waitJobState(t, job.ID, store.StateSucceeded, 5*time.Second)
	}
	// A pending approval: the list's one awaiting_approval row must carry its
	// fail-closed expiry on both faces — this is the field the phone countdown
	// renders, so the API==CLI contract is load-bearing for it.
	_, approval := h.gatedJob(t, 30*time.Minute)
	_ = approval

	// The list route. The CLI-side projection is the resolved form (expiry
	// stamped), so the byte contract covers the new field too.
	rows, err := h.store.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	want, err := wire.MarshalSuccess(wire.KindJobList, wire.NewJobViewsResolved(ctx, h.store, rows))
	if err != nil {
		t.Fatalf("MarshalSuccess: %v", err)
	}
	_, _, body := bearerGet(t, ts.URL, "/api/v1/jobs", token)
	if body != string(want) {
		t.Fatalf("list bytes differ from CLI projection:\n api: %q\n cli: %q", body, want)
	}
	// The awaiting_approval row actually carries the expiry (not a vacuous pass).
	if !strings.Contains(body, `"expires_at"`) {
		t.Fatal("the awaiting_approval job's expires_at is missing from the list")
	}

	// The single-resource route.
	job := rows[0]
	want, err = wire.MarshalSuccess(wire.KindJob, wire.NewJobViewResolved(ctx, h.store, job))
	if err != nil {
		t.Fatalf("MarshalSuccess get: %v", err)
	}
	_, _, body = bearerGet(t, ts.URL, "/api/v1/jobs/"+itoa64(job.ID), token)
	if body != string(want) {
		t.Fatalf("get bytes differ from CLI projection:\n api: %q\n cli: %q", body, want)
	}
}

// TestAPISecretsMetadataOnly seeds a secret with real nonce/ciphertext/AAD and
// asserts none of it appears in the /api/v1/secrets response — the route reuses
// the values-free SecretView, so a sealed value has no field to leak through.
func TestAPISecretsMetadataOnly(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	nonce := "NONCE_c0ffee_do_not_leak"
	ciphertext := "CIPHERTEXT_deadbeef_do_not_leak"
	aad := "AAD_binding_do_not_leak"
	if _, err := h.store.CreateSecret(context.Background(), store.SecretRow{
		Name:       "prod/deploy-key",
		Kind:       "ssh",
		Nonce:      []byte(nonce),
		Ciphertext: []byte(ciphertext),
		AAD:        aad,
		Source:     store.SecretSourceUser,
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	code, _, body := bearerGet(t, ts.URL, "/api/v1/secrets", token)
	if code != http.StatusOK {
		t.Fatalf("secrets status = %d, want 200", code)
	}
	// The metadata is present...
	if !strings.Contains(body, "prod/deploy-key") {
		t.Fatalf("secrets response missing the metadata name: %s", body)
	}
	// ...but not one byte of the sealed value, its nonce, or its AAD.
	for _, secret := range []string{nonce, ciphertext, aad} {
		if strings.Contains(body, secret) {
			t.Fatalf("secrets response leaked %q:\n%s", secret, body)
		}
	}
	// Guard the base64 encodings too: JSON marshals []byte as base64, so a naive
	// field would surface the value in that form, not as the raw string above.
	for _, enc := range []string{b64(nonce), b64(ciphertext)} {
		if strings.Contains(body, enc) {
			t.Fatalf("secrets response leaked base64 of a sealed field:\n%s", body)
		}
	}
}

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// TestAPIHealthReadonlyGetsWireEnvelope: a readonly token gets 200 and a wire
// envelope carrying the fixed set of subsystem entries doctor's JSON summary
// projects — the same probe definition, so the two can never diverge.
func TestAPIHealthReadonlyGetsWireEnvelope(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	code, hdr, body := bearerGet(t, ts.URL, "/api/v1/health", token)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/health status = %d, want 200 (body: %s)", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("GET /api/v1/health content-type = %q, want application/json", ct)
	}

	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Kind          string `json:"kind"`
		Data          []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("GET /api/v1/health body is not an envelope: %v (%s)", err, body)
	}
	if env.SchemaVersion != wire.SchemaVersion || env.Kind != wire.KindDoctor {
		t.Fatalf("bad envelope header: %+v", env)
	}
	wantSubsystems := []string{"data_dir", "secrets_protection", "optional_tools", "self_observability", "litestream", "privilege_posture"}
	if len(env.Data) != len(wantSubsystems) {
		t.Fatalf("subsystem count = %d, want %d: %s", len(env.Data), len(wantSubsystems), body)
	}
	for i, name := range wantSubsystems {
		if env.Data[i].Name != name {
			t.Errorf("entry %d name = %q, want %q (order must be stable): %s", i, env.Data[i].Name, name, body)
		}
	}
}

// TestAPIHealthNoTokenIsWireError: no token gets 401 and a wire error
// envelope, the same as every other /api/v1 route.
func TestAPIHealthNoTokenIsWireError(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	code, _, body := bearerGet(t, ts.URL, "/api/v1/health", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/health (no token) status = %d, want 401 (%s)", code, body)
	}
	var env struct {
		Kind  string     `json:"kind"`
		Error wire.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("no-token body is not a wire envelope: %v (%s)", err, body)
	}
	if env.Kind != wire.KindError || env.Error.Code != wire.CodeUnauthenticated {
		t.Fatalf("no-token envelope = %s, want error/unauthenticated", body)
	}
}

// TestAPIHealthNoScrapeTokenOrSecret seeds the real host key and scrape token
// a self-observability probe would present as a bearer, and proves neither it
// nor any sealed-column marker ever reaches the health response — the
// HealthEntry projection is values-free (invariant 4), same as doctor's JSON.
func TestAPIHealthNoScrapeTokenOrSecret(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	scrapeToken := metrics.ScrapeToken(h.env)
	if scrapeToken == "" {
		t.Fatal("expected a non-empty scrape token to check against")
	}

	code, _, body := bearerGet(t, ts.URL, "/api/v1/health", token)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/health status = %d, want 200 (%s)", code, body)
	}
	for _, banned := range []string{scrapeToken, "Bearer", "nonce", "ciphertext", "aad"} {
		if strings.Contains(body, banned) {
			t.Fatalf("health response leaked %q: %s", banned, body)
		}
	}
}

// TestAPIJobStreamSSE proves the API follow route streams the same
// text/event-stream body as the browser surface and ends on the terminal
// event. The job is already terminal before the stream opens, so the tail's
// first poll sees it, emits `done`, and returns — deterministic, no timing
// race with the queue worker.
func TestAPIJobStreamSSE(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	job, err := h.queue.Enqueue(context.Background(), queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	h.waitJobState(t, job.ID, store.StateSucceeded, 5*time.Second)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa64(job.ID)+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream content-type = %q, want text/event-stream", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !strings.Contains(string(body), "event: done") {
		t.Fatalf("stream on a terminal job never emitted done: %s", body)
	}
}

// TestAPIJobStreamNoTokenIsWireError: the SSE route rejects an unauthenticated
// request the same way every other /api/v1 route does — 401 and a wire error
// envelope, never the browser's 303-to-login.
func TestAPIJobStreamNoTokenIsWireError(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	code, hdr, body := bearerGet(t, ts.URL, "/api/v1/jobs/1/stream", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token stream status = %d, want 401", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("no-token stream content-type = %q, want application/json", ct)
	}
	var env struct {
		Kind  string     `json:"kind"`
		Error wire.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("no-token stream body is not a wire envelope: %v (%s)", err, body)
	}
	if env.Kind != wire.KindError || env.Error.Code != wire.CodeUnauthenticated {
		t.Fatalf("no-token stream envelope = %s, want error/unauthenticated", body)
	}
}

// TestAPIEventsSummary seeds events across every outcome and asserts the
// summary route tallies them correctly under the event.summary kind, reading
// through the same store.CountEventsByOutcome aggregation the handler calls.
func TestAPIEventsSummary(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")
	ctx := context.Background()

	seed := map[store.EventOutcome]int{
		store.OutcomeIgnored:    2,
		store.OutcomeMatched:    1,
		store.OutcomeSuppressed: 3,
		store.OutcomeGated:      1,
		store.OutcomeFired:      4,
		store.OutcomeError:      1,
	}
	for outcome, n := range seed {
		for i := 0; i < n; i++ {
			if _, err := h.store.CreateEvent(ctx, store.Event{
				Source: "alertmanager", Outcome: outcome,
			}); err != nil {
				t.Fatalf("create %s event: %v", outcome, err)
			}
		}
	}

	code, hdr, body := bearerGet(t, ts.URL, "/api/v1/events/summary", token)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/events/summary status = %d, want 200 (body: %s)", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("GET /api/v1/events/summary content-type = %q, want application/json", ct)
	}

	var env struct {
		SchemaVersion int                   `json:"schema_version"`
		Kind          string                `json:"kind"`
		Data          wire.EventSummaryView `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("GET /api/v1/events/summary body is not an envelope: %v (%s)", err, body)
	}
	if env.SchemaVersion != wire.SchemaVersion || env.Kind != wire.KindEventSummary {
		t.Fatalf("bad envelope header: %+v", env)
	}
	want := wire.EventSummaryView{
		Received: 12, Ignored: 2, Matched: 1, Suppressed: 3, Gated: 1, Fired: 4, Error: 1,
	}
	if env.Data != want {
		t.Fatalf("summary = %+v, want %+v (%s)", env.Data, want, body)
	}
}

// TestAPIEventsSummaryEmptyStore proves an empty store answers 200 with every
// count zero, not a 404 — the same "empty collection is still success" shape
// the list routes hold (TestAPIReadonlyGetsWireEnvelope).
func TestAPIEventsSummaryEmptyStore(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	code, _, body := bearerGet(t, ts.URL, "/api/v1/events/summary", token)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/events/summary (empty store) status = %d, want 200 (%s)", code, body)
	}
	var env struct {
		Data wire.EventSummaryView `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("body is not an envelope: %v (%s)", err, body)
	}
	if env.Data != (wire.EventSummaryView{}) {
		t.Fatalf("empty-store summary = %+v, want all zero", env.Data)
	}
}

// TestAPIEventsSummaryNoTokenIsWireError: no token gets 401 and a wire error
// envelope, the same as every other /api/v1 route.
func TestAPIEventsSummaryNoTokenIsWireError(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	code, hdr, body := bearerGet(t, ts.URL, "/api/v1/events/summary", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("no-token content-type = %q, want application/json", ct)
	}
	var env struct {
		Kind  string     `json:"kind"`
		Error wire.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("no-token body is not a wire envelope: %v (%s)", err, body)
	}
	if env.Kind != wire.KindError || env.Error.Code != wire.CodeUnauthenticated {
		t.Fatalf("no-token envelope = %s, want error/unauthenticated", body)
	}
}

// TestAPIJobsFilterAcceptsDegradedState asserts GET /api/v1/jobs?state=degraded
// is a valid filter: the read API must not refuse a real terminal state. The
// admitted set stays closed — an unknown state remains a 400.
func TestAPIJobsFilterAcceptsDegradedState(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	code, _, body := bearerGet(t, ts.URL, "/api/v1/jobs?state=degraded", token)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/jobs?state=degraded status = %d, want 200 (body: %s)", code, body)
	}

	code, _, body = bearerGet(t, ts.URL, "/api/v1/jobs?state=bogus", token)
	if code != http.StatusBadRequest {
		t.Fatalf("GET /api/v1/jobs?state=bogus status = %d, want 400 (body: %s)", code, body)
	}
}

// TestAPIJobPrivilegedField is the wire gate: a job that ran unsandboxed
// carries privileged:true in its job.get body over the API, so an agent reading
// a job can see it; a sandboxed job omits the field. Dropping the projection
// must red.
func TestAPIJobPrivilegedField(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "agent", "readonly")

	priv := seedJobWithRecap(t, h, store.StateSucceeded, 0, nil, nil,
		store.JobArgs{Argv: []string{"/bin/true"}, Privileged: true})
	code, _, body := bearerGet(t, ts.URL, "/api/v1/jobs/"+itoa64(priv.ID), token)
	if code != http.StatusOK {
		t.Fatalf("privileged job get = %d, want 200 (%s)", code, body)
	}
	if !strings.Contains(body, `"privileged":true`) {
		t.Errorf("privileged job get missing privileged:true:\n%s", body)
	}

	plain := seedJobWithRecap(t, h, store.StateSucceeded, 0, nil, nil,
		store.JobArgs{Argv: []string{"/bin/true"}})
	_, _, plainBody := bearerGet(t, ts.URL, "/api/v1/jobs/"+itoa64(plain.ID), token)
	if strings.Contains(plainBody, `"privileged"`) {
		t.Errorf("sandboxed job get must omit privileged:\n%s", plainBody)
	}
}
