package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
			if r.CreatedBy != "token laptop via web" {
				t.Fatalf("created by = %q, want the browser session's token", r.CreatedBy)
			}
		}
	}
	if !found {
		t.Fatal("schedule was not created")
	}
}

// TestScheduleFormAtRest renders the schedules-add dialog for an operator and
// asserts the forms-round anatomy: the reusable field pattern with
// always-present meta lines (help at rest), the kind select's own meta line,
// aria wiring tying each control to its meta, a form-status line, and
// novalidate so the daemon stays authoritative.
func TestScheduleFormAtRest(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, op, ts.URL+"/schedules")
	if code != http.StatusOK {
		t.Fatalf("/schedules = %d, want 200", code)
	}
	for _, want := range []string{
		`id="schedules-add"`,
		`action="/schedules/add"`,
		`id="schedule-add-form" novalidate`,
		`aria-describedby="sc-name-meta"`,
		`aria-describedby="sc-kind-meta"`,
		`aria-describedby="sc-cron-meta"`,
		`aria-describedby="sc-argv-meta"`,
		`aria-describedby="sc-target-meta"`,
		`aria-describedby="sc-ck-meta"`,
		`aria-describedby="sc-jitter-meta"`,
		`id="sc-name-meta"`,
		`id="sc-kind-meta"`,
		`id="sc-cron-meta"`,
		`id="sc-argv-meta"`,
		`id="sc-target-meta"`,
		`id="sc-ck-meta"`,
		`id="sc-jitter-meta"`,
		"Letters, digits, dots, hyphens, underscores.",
		"Kind is shell, backup, play or drill.",
		"A 5-field cron or an @every",
		"The command (shell) or playbook command (play).",
		"A whole number of seconds — like 300.",
		`role="status" aria-live="polite"`,
		"Add schedule",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("schedule form missing %q", want)
		}
	}
	// At rest no control carries a refusal and the metas are plain help text.
	if !strings.Contains(page, `placeholder="nightly-backup" aria-describedby="sc-name-meta">`) {
		t.Error("at-rest name control carries an aria-invalid refusal")
	}
	if !strings.Contains(page, `id="sc-name-meta" class="meta">Letters`) {
		t.Error("at-rest name meta is not plain help text")
	}
}

// TestScheduleFormRefusalsRender asserts the 422 re-render fills every meta
// line at once (name, cron, argv, jitter), keeps the typed values, and never
// flashes: a refusal is the form's own surface, flash is success-only.
func TestScheduleFormRefusalsRender(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/schedules/add",
		url.Values{
			"name": {"bad name!"}, "cron": {"not a cron expr"}, "kind": {"shell"},
			"argv": {""}, "jitter": {"abc"},
		}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("refusal status = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{
		`aria-invalid="true"`,
		"Names can&#39;t have spaces.",
		"Give a 5-field cron or an @every — like 0 */5 * * * * or @every 5m.",
		"Give a command — shell and play kinds need one.",
		"The jitter is a whole number of seconds — like 300.",
		`value="bad name!"`,
		`exoModalOpen("schedules-add")`,
		`autofocus`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("422 re-render missing %q", want)
		}
	}
	// A refusal never flashes: the render consumes the flash cookie (an empty
	// clearing Set-Cookie), so no flash MESSAGE may ride this response.
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("schedule refusal carried a flash message")
		}
	}
}

// TestScheduleFormConcurrencyRefused asserts the backup/drill concurrency-key
// refusal lands on the concurrency field's meta line (schedules_write.go's
// old kind-message flash).
func TestScheduleFormConcurrencyRefused(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/schedules/add",
		url.Values{"name": {"nightly"}, "cron": {"@every 24h"}, "kind": {"backup"},
			"concurrency_key": {"lock"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("concurrency refusal status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "the concurrency key does not apply") {
		t.Errorf("concurrency refusal missing the field sentence")
	}
	if _, err := h.store.GetSchedule(context.Background(), "nightly"); err == nil {
		t.Fatal("a schedule was created despite a disallowed concurrency key")
	}
}

// TestScheduleAddValidatesCron: an unparseable cron expression is refused
// before the store sees it; the form re-renders at 422 with the cron field's
// sentence, and no row lands.
func TestScheduleAddValidatesCron(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/schedules/add",
		url.Values{"name": {"bad"}, "cron": {"not a cron expr"}, "kind": {"shell"}, "argv": {"true"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("cron refusal status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Give a 5-field cron or an @every") {
		t.Errorf("cron refusal missing the cron-field sentence")
	}
	if _, err := h.store.GetSchedule(context.Background(), "bad"); err == nil {
		t.Fatal("a schedule was created despite an invalid cron expression")
	}
}

// TestScheduleAddShellNeedsCommand: kind shell with no command re-renders the
// form at 422 with the argv field's sentence, and no row lands.
func TestScheduleAddShellNeedsCommand(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, op, ts.URL+"/schedules/add",
		url.Values{"name": {"empty-shell"}, "cron": {"@every 5m"}, "kind": {"shell"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("argv refusal status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Give a command — shell and play kinds need one.") {
		t.Errorf("argv refusal missing the argv-field sentence")
	}
	if _, err := h.store.GetSchedule(context.Background(), "empty-shell"); err == nil {
		t.Fatal("a schedule was created despite a missing shell command")
	}
}

// TestScheduleAddRefusesBadName: a name the ValidName gate refuses re-renders
// the form at 422 with the name field's human sentence — never the banned
// "invalid schedule name" — and no row lands.
func TestScheduleAddRefusesBadName(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	for _, name := range []string{"", "bad name!", "a/b"} {
		resp := postForm(t, op, ts.URL+"/schedules/add",
			url.Values{"name": {name}, "cron": {"@every 5m"}, "kind": {"shell"}, "argv": {"true"}}, nil)
		body := readBody(t, resp)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("name %q: status = %d, want 422", name, resp.StatusCode)
		}
		if strings.Contains(body, "invalid schedule name") {
			t.Errorf("name %q: refusal leaks the banned old copy", name)
		}
		if !strings.Contains(body, "Name") {
			t.Errorf("name %q: refusal missing the name-field sentence", name)
		}
		if _, err := h.store.GetSchedule(context.Background(), name); err == nil {
			t.Fatalf("name %q: a schedule was created despite an invalid name", name)
		}
	}
}

// TestScheduleAddDuplicateRefused: re-adding a name that exists re-renders the
// form at 422 with the duplicate sentence on the name field, and the original
// row survives.
func TestScheduleAddDuplicateRefused(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := login(t, ts.URL, h.mint(t, "laptop", "operator"))
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, newTogglerSchedule()); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	resp := postForm(t, op, ts.URL+"/schedules/add",
		url.Values{"name": {"toggler"}, "cron": {"@every 5m"}, "kind": {"shell"}, "argv": {"true"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "A schedule named toggler already exists.") {
		t.Errorf("duplicate 422 missing the duplicate sentence")
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

// TestScheduleRmRefusalOnRow: a confirmed-less remove re-renders the schedules
// surface at 422 with the refusal on the row — the forms/SPEC.md button-verb
// shape — never a flash, and nothing is removed.
func TestScheduleRmRefusalOnRow(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, newTogglerSchedule()); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	resp := postForm(t, client, ts.URL+"/schedules/toggler/rm", url.Values{}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("rm refusal status = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{
		`form-status is-error`,
		"Nothing was removed — confirm the removal in the dialog.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rm refusal missing %q", want)
		}
	}
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("rm refusal flashed")
		}
	}
	if _, err := h.store.GetSchedule(ctx, "toggler"); err != nil {
		t.Fatal("refused rm removed the schedule")
	}
}

// TestScheduleRmGoneRefuses: removing a schedule that is already gone keeps
// the appropriate 404 with human copy — never a flash — and the surface is
// not crashed.
func TestScheduleRmGoneRefuses(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/schedules/nope/rm", url.Values{"confirmed": {"1"}}, nil)
	body := readBody(t, resp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rm gone status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body, "No schedule named nope exists to remove.") {
		t.Errorf("rm gone refusal missing the on-surface sentence")
	}
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && c.Value != "" {
			t.Error("rm gone refusal flashed")
		}
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

func TestScheduleRunReadonlyForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ro := h.mint(t, "ro", "readonly")
	client := login(t, ts.URL, ro)

	resp := postForm(t, client, ts.URL+"/schedules/nightly/run", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly schedule run = %d, want 403", resp.StatusCode)
	}
}

func TestScheduleRunCrossOriginForbidden(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/schedules/nightly/run",
		url.Values{},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin schedule run = %d, want 403", resp.StatusCode)
	}
}

func TestScheduleRunNotFound(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	resp := postForm(t, client, ts.URL+"/schedules/nonexistent/run", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nonexistent schedule run = %d, want 404", resp.StatusCode)
	}
}

func TestScheduleRunEnqueuesAndRedirects(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly-shell", CronExpr: "0 0 * * *", Kind: store.ScheduleShell,
		Args: store.ScheduleArgs{Argv: []string{"echo", "scheduled"}},
	}); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	// Verify row action appears on /schedules
	code, page := getBody(t, client, ts.URL+"/schedules")
	if code != http.StatusOK {
		t.Fatalf("get /schedules = %d, want 200", code)
	}
	if !strings.Contains(page, `action="/schedules/nightly-shell/run"`) {
		t.Errorf("/schedules missing run action for nightly-shell: %s", page)
	}

	resp := postForm(t, client, ts.URL+"/schedules/nightly-shell/run", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("schedule run status = %d, want 303", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/jobs/") {
		t.Fatalf("Location = %q, want /jobs/{id}", loc)
	}
	jobIDStr := strings.TrimPrefix(loc, "/jobs/")

	var foundFlash bool
	for _, c := range resp.Cookies() {
		if c.Name == flashCookie && strings.Contains(c.Value, "enqueued nightly-shell as job #"+jobIDStr) {
			foundFlash = true
		}
	}
	if !foundFlash {
		t.Error("schedule run did not set flash cookie")
	}

	// Verify enqueued job in store
	jobs, err := h.store.ListJobs(ctx, store.JobFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	var matched bool
	for _, j := range jobs {
		if j.Runbook == "nightly-shell" {
			matched = true
			if j.Trigger != "operator" {
				t.Errorf("job trigger = %q, want operator", j.Trigger)
			}
			if len(j.Args.Argv) != 2 || j.Args.Argv[0] != "echo" || j.Args.Argv[1] != "scheduled" {
				t.Errorf("job argv = %v, want [echo scheduled]", j.Args.Argv)
			}
		}
	}
	if !matched {
		t.Error("enqueued job not found in store")
	}
}

// TestScheduleRunFromSourceInheritsItsDefaults: Run now on a play schedule that
// names a source builds the same spec a fire does, so the job runs from the
// source's checkout and carries its default egress. A schedule whose source has
// gone is refused on its row rather than run without the defaults.
func TestScheduleRunFromSourceInheritsItsDefaults(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	ctx := context.Background()

	invPath := filepath.Join(t.TempDir(), "edge.json")
	if err := os.WriteFile(invPath, []byte(`{"all":{"hosts":{"edge1":{"ansible_host":"10.0.0.1"}}}}`), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	if _, err := h.store.MaterializeInventory(ctx, "tf:edge", invPath); err != nil {
		t.Fatalf("materialize inventory: %v", err)
	}
	checkout := t.TempDir()
	src, err := h.store.CreateSource(ctx, store.Source{Name: "infra", Kind: store.SourcePath, URI: checkout, Enabled: true})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := h.store.SetSourceEgressPorts(ctx, src.ID, []int{443}); err != nil {
		t.Fatalf("set egress: %v", err)
	}
	for _, name := range []string{"converge", "orphan"} {
		source := "infra"
		if name == "orphan" {
			source = "gone"
		}
		if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
			Name: name, CronExpr: "@every 6h", Kind: "play",
			Args: store.ScheduleArgs{Argv: []string{"site.yml"}, Target: "tf:edge", Source: source},
		}); err != nil {
			t.Fatalf("seed schedule %s: %v", name, err)
		}
	}

	resp := postForm(t, client, ts.URL+"/schedules/converge/run", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("run converge = %d, want 303", resp.StatusCode)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(resp.Header.Get("Location"), "/jobs/"), 10, 64)
	if err != nil {
		t.Fatalf("Location %q is not a job: %v", resp.Header.Get("Location"), err)
	}
	job, err := h.store.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Args.Source != "infra" || job.Args.Dir != checkout {
		t.Errorf("job source/dir = %q/%q, want infra and %q", job.Args.Source, job.Args.Dir, checkout)
	}
	if len(job.Args.EgressPorts) != 1 || job.Args.EgressPorts[0] != 443 {
		t.Errorf("job egress = %v, want the source default [443]", job.Args.EgressPorts)
	}

	resp = postForm(t, client, ts.URL+"/schedules/orphan/run", url.Values{}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("run orphan = %d, want 422 for a schedule whose source is gone", resp.StatusCode)
	}
}
