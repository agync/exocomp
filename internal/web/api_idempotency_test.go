package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestIdempotencyKeyIsScopedToPrincipalAndRoute proves an idempotency key
// cannot cross principals: the header used to be one global namespace, so a
// planted key silently swallowed another token's write — the victim got a
// 200 naming the attacker's job, its own apply never enqueued, and its name
// appended to a ledger it did not create. The key is now namespaced by the
// authenticated token and the route.
func TestIdempotencyKeyIsScopedToPrincipalAndRoute(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()
	h.seedWorkspace(t, "web")

	attacker := h.mint(t, "attacker", "operator")
	victim := h.mint(t, "victim", "operator")
	hdr := map[string]string{"Idempotency-Key": "deploy-2026-08-23"}

	planned := postJobID(t, ts.URL, "/api/v1/workspaces/web/plan", attacker, hdr)
	applied := postJobID(t, ts.URL, "/api/v1/workspaces/web/apply", victim, hdr)
	if applied == planned {
		t.Fatalf("the victim's apply returned the attacker's job %d — one global key namespace", planned)
	}
	job, err := h.store.GetJob(ctx, applied)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if argv := strings.Join(job.Args.Argv, " "); !strings.Contains(argv, "apply") {
		t.Fatalf("the victim's job argv = %q, want an apply", argv)
	}
	// The attacker's job carries only its own attribution.
	events, err := h.store.ListJobEvents(ctx, planned)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, e := range events {
		if strings.Contains(e.Msg, "victim") {
			t.Fatalf("a foreign write wrote attribution onto job %d: %q", planned, e.Msg)
		}
	}

	// One token, one key, two routes: still two jobs.
	same := postJobID(t, ts.URL, "/api/v1/workspaces/web/apply", attacker, hdr)
	if same == planned {
		t.Fatalf("the same key on a different route reused job %d", planned)
	}
	// And the dedup a retry depends on still holds: same token, route, and key.
	if again := postJobID(t, ts.URL, "/api/v1/workspaces/web/apply", attacker, hdr); again != same {
		t.Fatalf("a retry enqueued a second job: %d then %d", same, again)
	}

	jobs, err := h.store.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("store holds %d jobs, want 3 (attacker plan, victim apply, attacker apply)", len(jobs))
	}
}

func postJobID(t *testing.T, base, path, token string, hdr map[string]string) int64 {
	t.Helper()
	code, _, body := bearerPost(t, base, path, token, "", hdr)
	if code != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200 (%s)", path, code, body)
	}
	var jv jobEnqueuedView
	decodeData(t, body, &jv)
	if jv.JobID == 0 {
		t.Fatalf("POST %s returned no job id (%s)", path, body)
	}
	return jv.JobID
}
