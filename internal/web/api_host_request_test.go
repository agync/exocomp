package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

func TestAPIHostRequestEnqueues(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	if _, err := h.store.CreateRule(context.Background(), store.NewRule{
		Name:         "host-ping",
		SourceFilter: reactive.SourceHost,
		CELExpr:      `event.name == "ping"`,
		ActionKind:   store.ActionShell,
		Action:       store.RuleAction{Argv: []string{"/bin/sh", "-c", "echo ok; exit 0"}},
	}); err != nil {
		t.Fatal(err)
	}

	token, _, err := MintToken(context.Background(), h.store, "host-node", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}

	code, _, body := bearerPost(t, ts.URL, "/api/v1/hosts/self/requests", token,
		`{"request":"ping"}`, nil)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", code, body)
	}
	var env struct {
		Data struct {
			Outcome string `json:"outcome"`
			JobID   *int64 `json:"job_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Outcome != "fired" || env.Data.JobID == nil {
		t.Fatalf("outcome = %q job_id = %v, want fired with job", env.Data.Outcome, env.Data.JobID)
	}
}

func TestAPIHostRequestRejectsUnknownRequest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	token, _, err := MintToken(context.Background(), h.store, "host-node2", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}
	code, _, body := bearerPost(t, ts.URL, "/api/v1/hosts/self/requests", token,
		`{"request":"bogus"}`, nil)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", code, body)
	}
	if !strings.Contains(body, `"ignored"`) {
		t.Fatalf("expected ignored outcome, got %s", body)
	}
}

func TestAPIHostRequestScopeIsolated(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()

	// A host token must not be able to read the job list.
	token, _, err := MintToken(context.Background(), h.store, "host-node3", store.ScopeHostToken, "node.lan", 0)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /jobs with host token status = %d, want 403", resp.StatusCode)
	}
}
