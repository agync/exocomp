package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

func TestScheduleWriteReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/schedules/add",
		url.Values{"name": {"nightly"}, "cron": {"@every 5m"}, "kind": {"shell"}, "argv": {"true"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly schedule add = %d, want 403", resp.StatusCode)
	}
}

func TestScheduleWriteCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/schedules/add",
		url.Values{"name": {"nightly"}, "cron": {"@every 5m"}, "kind": {"shell"}, "argv": {"true"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin schedule add = %d, want 403", resp.StatusCode)
	}
}

func TestScheduleAddCreates(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	resp := postForm(t, client, ts.URL+"/schedules/add",
		url.Values{"name": {"nightly-backup"}, "cron": {"@every 24h"}, "kind": {"backup"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("schedule add = %d, want 303", resp.StatusCode)
	}

	rows, err := h.store.ListSchedules(ctx, false)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Name == "nightly-backup" {
			found = true
			if r.Kind != "backup" {
				t.Fatalf("kind = %q, want backup", r.Kind)
			}
			if !r.Enabled {
				t.Fatal("newly created schedule should be enabled")
			}
		}
	}
	if !found {
		t.Fatal("schedule was not created")
	}
}

// TestScheduleAddValidatesCron: an unparseable cron expression is refused
// before the store sees it, the rejection resolves as a flash-carrying
// redirect, and no row lands.
func TestScheduleAddValidatesCron(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/schedules/add",
		url.Values{"name": {"bad"}, "cron": {"not a cron expr"}, "kind": {"shell"}, "argv": {"true"}}, nil)
	msg := flashMessage(t, resp, "/schedules")
	_ = resp.Body.Close()
	if !strings.Contains(msg, "not a cron expression") {
		t.Errorf("flash message %q does not report the cron error", msg)
	}
	if _, err := h.store.GetSchedule(context.Background(), "bad"); err == nil {
		t.Fatal("a schedule was created despite an invalid cron expression")
	}
}

// TestScheduleAddShellNeedsCommand: kind shell with no command is refused,
// the rejection resolves as a flash-carrying redirect, and no row lands.
func TestScheduleAddShellNeedsCommand(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/schedules/add",
		url.Values{"name": {"empty-shell"}, "cron": {"@every 5m"}, "kind": {"shell"}}, nil)
	msg := flashMessage(t, resp, "/schedules")
	_ = resp.Body.Close()
	if !strings.Contains(msg, "needs a command") {
		t.Errorf("flash message %q does not report the missing command", msg)
	}
	if _, err := h.store.GetSchedule(context.Background(), "empty-shell"); err == nil {
		t.Fatal("a schedule was created despite a missing shell command")
	}
}

// TestScheduleAddRefusesBadName: a name the ValidName gate refuses is
// refused before the store sees it — with a flash-carrying redirect, not a bare
// 500, and no row lands. The same guard surfaces through `schedule add`,
// so a schedule that could never be addressed by the web cannot be created there
// either.
func TestScheduleAddRefusesBadName(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	for _, name := range []string{"", "bad name!", "a/b"} {
		resp := postForm(t, client, ts.URL+"/schedules/add",
			url.Values{"name": {name}, "cron": {"@every 5m"}, "kind": {"shell"}, "argv": {"true"}}, nil)
		msg := flashMessage(t, resp, "/schedules")
		_ = resp.Body.Close()
		if !strings.Contains(msg, "invalid schedule name") {
			t.Errorf("name %q: flash message %q does not report the name refusal", name, msg)
		}
		if _, err := h.store.GetSchedule(context.Background(), name); err == nil {
			t.Fatalf("name %q: a schedule was created despite an invalid name", name)
		}
	}
}

// TestScheduleAddDuplicateFlashes: re-adding a name that exists is refused
// with a flash-carrying redirect, not a 409, and the original row survives.
func TestScheduleAddDuplicateFlashes(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, newTogglerSchedule()); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/schedules/add",
		url.Values{"name": {"toggler"}, "cron": {"@every 5m"}, "kind": {"shell"}, "argv": {"true"}}, nil)
	msg := flashMessage(t, resp, "/schedules")
	_ = resp.Body.Close()
	if !strings.Contains(msg, "already exists") {
		t.Errorf("flash message %q does not report the duplicate", msg)
	}
	if _, err := h.store.GetSchedule(ctx, "toggler"); err != nil {
		t.Fatalf("original schedule vanished: %v", err)
	}
}

func TestScheduleEnableDisable(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, newTogglerSchedule()); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/schedules/toggler/disable", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("disable = %d, want 303", resp.StatusCode)
	}
	sc, err := h.store.GetSchedule(ctx, "toggler")
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if sc.Enabled {
		t.Fatal("schedule still enabled after disable")
	}

	resp = postForm(t, client, ts.URL+"/schedules/toggler/enable", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("enable = %d, want 303", resp.StatusCode)
	}
	sc, err = h.store.GetSchedule(ctx, "toggler")
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if !sc.Enabled {
		t.Fatal("schedule still disabled after enable")
	}
}

func TestScheduleRm(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, newTogglerSchedule()); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/schedules/toggler/rm", url.Values{"confirmed": {"1"}}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("rm = %d, want 303", resp.StatusCode)
	}
	if _, err := h.store.GetSchedule(ctx, "toggler"); err == nil {
		t.Fatal("schedule still present after rm")
	}
}

// TestScheduleDetailModalRendersSpec: the Schedules page renders each row's
// full spec into a per-row modal — reachable by a readonly identity, since
// viewing a schedule's spec needs only auth, not an operator token.
func TestScheduleDetailModalRendersSpec(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly-drift", CronExpr: "@every 6h", Kind: store.ScheduleShell,
		Args:           store.ScheduleArgs{Argv: []string{"echo", "hi"}, Target: "prod"},
		ConcurrencyKey: "drift-lock", JitterS: 30,
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	code, body := getBody(t, client, ts.URL+"/schedules")
	if code != http.StatusOK {
		t.Fatalf("GET /schedules = %d, want 200", code)
	}
	if !strings.Contains(body, `data-modal-row="sched-nightly-drift"`) {
		t.Error("schedule row is not wired to open its spec modal")
	}
	if !strings.Contains(body, `id="sched-nightly-drift"`) {
		t.Error("schedule spec modal was not rendered")
	}
	if !strings.Contains(body, "@every 6h") {
		t.Error("modal does not render the cron expression")
	}
	if !strings.Contains(body, "echo hi") {
		t.Error("modal does not render the command")
	}
	if !strings.Contains(body, store.ScheduleShell) {
		t.Error("modal does not render the kind")
	}
	if !strings.Contains(body, "drift-lock") {
		t.Error("modal does not render the concurrency key")
	}
}

// newTogglerSchedule is a minimal valid shell schedule shared by the
// enable/disable/rm tests, which do not exercise /schedules/add itself.
func newTogglerSchedule() store.NewSchedule {
	return store.NewSchedule{
		Name: "toggler", CronExpr: "@every 5m", Kind: store.ScheduleShell,
		Args: store.ScheduleArgs{Argv: []string{"true"}},
	}
}

// TestSchedulesFromJobPrefill: GET /schedules?from_job=<id> prefills the add
// form's kind/argv from that job's own stored Argv, and auto-opens the modal.
func TestSchedulesFromJobPrefill(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	job, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "playbook", Kind: executor.KindAnsible, Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"ansible-playbook", "site.yml"}},
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}

	_, body := getBody(t, client, ts.URL+"/schedules?from_job="+itoa64(job.ID))
	if !strings.Contains(body, `value="ansible-playbook site.yml"`) {
		t.Errorf("argv field not prefilled from job, body:\n%s", body)
	}
	if !strings.Contains(body, `option value="shell" selected`) {
		t.Error("kind select did not default to shell for the derived prefill")
	}
	if !strings.Contains(body, `exoModalOpen("schedules-add")`) {
		t.Error("add-schedule modal did not auto-open for a from_job deep link")
	}
}

// TestSchedulesFromJobPrefillIgnoresUnschedulable: a from_job id that does not
// resolve to a schedulable job (here, a tofu job) is ignored, not an error —
// the operator lands on a plain, unprefilled add form.
func TestSchedulesFromJobPrefillIgnoresUnschedulable(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	job, err := h.store.CreateJob(context.Background(), store.NewJob{
		Runbook: "prod-plan", Kind: executor.KindTofu, Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"tofu", "plan"}},
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}

	code, body := getBody(t, client, ts.URL+"/schedules?from_job="+itoa64(job.ID))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if strings.Contains(body, `exoModalOpen("schedules-add")`) {
		t.Error("modal should not auto-open for an unschedulable from_job")
	}
}

// TestSchedulesExplicitPrefill: explicit ?kind=&argv=&target= query params
// prefill the add form directly, without a from_job lookup.
func TestSchedulesExplicitPrefill(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	_, body := getBody(t, client, ts.URL+"/schedules?kind=play&argv=site.yml&target=fleet&name=nightly")
	if !strings.Contains(body, `value="nightly"`) {
		t.Error("name field not prefilled")
	}
	if !strings.Contains(body, `value="site.yml"`) {
		t.Error("argv field not prefilled")
	}
	if !strings.Contains(body, `value="fleet"`) {
		t.Error("target field not prefilled")
	}
	if !strings.Contains(body, `option value="play" selected`) {
		t.Error("kind select did not honor the explicit kind param")
	}
}
