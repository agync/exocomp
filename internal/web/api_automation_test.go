package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIScheduleAndRuleRoutes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")

	runAPICalls(t, ts.URL, op, []apiCall{
		{"schedule", http.MethodPost, "/api/v1/schedules", `{"name":"tick","cron":"@every 1h","kind":"shell","argv":["true"]}`,
			http.StatusOK, `"cron":"@every 1h"`},
		{"duplicate", http.MethodPost, "/api/v1/schedules", `{"name":"tick","cron":"@every 1h","kind":"shell","argv":["true"]}`,
			http.StatusConflict, "duplicate"},
		{"bad cron", http.MethodPost, "/api/v1/schedules", `{"name":"x","cron":"nope","kind":"shell","argv":["true"]}`,
			http.StatusBadRequest, "cron"},
		{"backup argv", http.MethodPost, "/api/v1/schedules", `{"name":"x","cron":"@every 1h","kind":"backup","argv":["x"]}`,
			http.StatusBadRequest, "takes no argv"},
		{"play no target", http.MethodPost, "/api/v1/schedules", `{"name":"x","cron":"@every 1h","kind":"play","argv":["site.yml"]}`,
			http.StatusBadRequest, "target"},
		{"play unknown source", http.MethodPost, "/api/v1/schedules",
			`{"name":"x","cron":"@every 1h","kind":"play","argv":["site.yml"],"target":"fleet","source":"absent"}`,
			http.StatusBadRequest, `no source \"absent\"`},
		{"disable", http.MethodPost, "/api/v1/schedules/tick/disable", "", http.StatusOK, `"enabled":false`},
		{"enable unknown", http.MethodPost, "/api/v1/schedules/nope/enable", "", http.StatusNotFound, "not_found"},
		{"rm", http.MethodDelete, "/api/v1/schedules/tick", "", http.StatusOK, `"name":"tick"`},
		{"rule", http.MethodPost, "/api/v1/rules", `{"name":"r","expr":"true","action":"shell","argv":["true"]}`,
			http.StatusOK, `"action_kind":"shell"`},
		{"play options", http.MethodPost, "/api/v1/schedules",
			`{"name":"p","cron":"@every 1h","kind":"play","argv":["/srv/site.yml","--private-key=/etc/k"],"target":"fleet"}`,
			http.StatusBadRequest, "--local"},
		{"bad expr", http.MethodPost, "/api/v1/rules", `{"name":"r2","expr":"event.(","action":"notify"}`,
			http.StatusBadRequest, ""},
		{"notify gate", http.MethodPost, "/api/v1/rules", `{"name":"r3","expr":"true","action":"notify","require_approval":true}`,
			http.StatusBadRequest, "nothing to gate"},
		{"identity not allowed", http.MethodPost, "/api/v1/rules",
			`{"name":"r4","expr":"true","action":"shell","argv":["true"],"ssh_identity_files":["/srv/key"]}`,
			http.StatusBadRequest, "not allowed"},
		{"rule disable", http.MethodPost, "/api/v1/rules/r/disable", "", http.StatusOK, `"enabled":false`},
		{"rule rm", http.MethodDelete, "/api/v1/rules/r", "", http.StatusOK, `"name":"r"`},
	})
}

// A rule list is read at the readonly floor, so a shell action's argv, which
// may carry a value, stays out of it.
func TestAPIRuleListOmitsShellArgv(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")
	ro := h.mint(t, "ro", "readonly")

	runAPICalls(t, ts.URL, op, []apiCall{
		{"rule", http.MethodPost, "/api/v1/rules",
			`{"name":"r","expr":"true","action":"shell","argv":["notify","--token=hunter2"]}`, http.StatusOK, ""},
	})
	code, _, body := bearerGet(t, ts.URL, "/api/v1/rules", ro)
	if code != http.StatusOK || !strings.Contains(body, `"name":"r"`) {
		t.Fatalf("readonly rule list = %d %s", code, body)
	}
	if strings.Contains(body, "hunter2") || strings.Contains(body, `"argv"`) {
		t.Fatalf("readonly rule list carries the shell argv: %s", body)
	}
}

// A schedule or rule created over the API records the token that created it,
// in the ledger's words, and the readonly lists show it.
func TestAPIScheduleAndRuleRecordTheirCreator(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")
	ro := h.mint(t, "ro", "readonly")

	runAPICalls(t, ts.URL, op, []apiCall{
		{"schedule", http.MethodPost, "/api/v1/schedules", `{"name":"tick","cron":"@every 1h","kind":"shell","argv":["true"]}`,
			http.StatusOK, `"created_by":"token op via api"`},
		{"rule", http.MethodPost, "/api/v1/rules", `{"name":"r","expr":"true","action":"shell","argv":["true"]}`,
			http.StatusOK, `"created_by":"token op via api"`},
	})
	ctx := context.Background()
	if sc, err := h.store.GetSchedule(ctx, "tick"); err != nil || sc.CreatedBy != "token op via api" {
		t.Fatalf("stored schedule = %+v (%v), want its creator recorded", sc, err)
	}
	if r, err := h.store.GetRule(ctx, "r"); err != nil || r.CreatedBy != "token op via api" {
		t.Fatalf("stored rule = %+v (%v), want its creator recorded", r, err)
	}
	for _, path := range []string{"/api/v1/schedules", "/api/v1/rules"} {
		code, _, body := bearerGet(t, ts.URL, path, ro)
		if code != http.StatusOK || !strings.Contains(body, `"created_by":"token op via api"`) {
			t.Fatalf("readonly %s = %d %s, want the creator shown", path, code, body)
		}
	}
}

// A rule written over the API cannot widen itself past what the CLI offers: a
// field the route does not take is refused, not dropped.
func TestAPIRuleRefusesCrossHost(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "op", "operator")

	runAPICalls(t, ts.URL, op, []apiCall{
		{"rule", http.MethodPost, "/api/v1/rules", `{"name":"r","expr":"true","action":"shell","argv":["true"],"allow_cross_host":true}`,
			http.StatusBadRequest, "allow_cross_host"},
	})
	rules, err := h.store.ListRules(context.Background(), false)
	if err != nil || len(rules) != 0 {
		t.Fatalf("rules = %+v (%v), want none", rules, err)
	}
}

func TestAPIScheduleAndRuleRoutesNeedAnOperatorBearer(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")
	runAPICalls(t, ts.URL, ro, []apiCall{
		{"schedule", http.MethodPost, "/api/v1/schedules", `{"name":"tick","cron":"@every 1h","kind":"shell","argv":["true"]}`,
			http.StatusForbidden, "operator"},
		{"rule", http.MethodPost, "/api/v1/rules", `{"name":"r","expr":"true","action":"notify"}`, http.StatusForbidden, "operator"},
	})
}
