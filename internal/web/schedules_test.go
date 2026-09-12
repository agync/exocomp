package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestScheduleRowsHaveDetailsButton: every schedule row carries a focusable
// Details button that opens the same per-row spec modal the row's mouse click
// opens — so a keyboard operator can reach the spec without a pointer.
func TestScheduleRowsHaveDetailsButton(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly-drift", CronExpr: "@every 6h", Kind: store.ScheduleShell,
		Args: store.ScheduleArgs{Argv: []string{"echo", "hi"}},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/schedules")
	if code != http.StatusOK {
		t.Fatalf("GET /schedules = %d, want 200", code)
	}
	for _, want := range []string{
		`data-modal-row="sched-nightly-drift"`,
		`id="sched-nightly-drift"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/schedules missing %q", want)
		}
	}
	// The inline Details button is gone (W8): the row itself opens the modal.
	if strings.Contains(page, ">Details</button>") {
		t.Error("/schedules still renders the redundant inline Details button (W8)")
	}
}

// TestScheduleSpecModalKeyboardPath pins the MED fix: the row's click
// affordance has no tab stop, so the schedule name cell carries a real button
// that opens the same spec dialog — a keyboard operator (Tab to the name,
// Enter/Space) can reach the modal without a pointer, and the native dialog's
// own focus handling covers focus-on-open and Esc.
func TestScheduleSpecModalKeyboardPath(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly", CronExpr: "@every 6h", Kind: store.ScheduleShell,
		Args: store.ScheduleArgs{Argv: []string{"echo", "hi"}},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/schedules")
	if code != http.StatusOK {
		t.Fatalf("GET /schedules = %d, want 200", code)
	}
	if !strings.Contains(page, `<button type="button" class="cellact" data-modal-open="sched-nightly">nightly</button>`) {
		t.Error("schedule spec dialog has no keyboard-reachable trigger (MED)")
	}
	if !strings.Contains(page, `id="sched-nightly"`) {
		t.Error("schedule spec dialog missing")
	}
}

// TestScheduleRowsLinkBackToBackupTarget: a kind=backup schedule row links
// back to the target it fires (A5): the row's target cell renders the target
// name as a link to /system/backups — the reverse of the backups SCHEDULE
// cell's link to /schedules, so the linkage reads both ways.
func TestScheduleRowsLinkBackToBackupTarget(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly-backup", CronExpr: "@every 24h", Kind: store.ScheduleBackup,
		Args: store.ScheduleArgs{Target: "nas01"},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/schedules")
	if code != http.StatusOK {
		t.Fatalf("GET /schedules = %d, want 200", code)
	}
	if !strings.Contains(page, `href="/system/backups" class="mono">nas01</a>`) {
		t.Errorf("/schedules backup row missing the back-link to its target:\n%s", page)
	}
}

// TestScheduleRowsLinkBackToDrillTarget: a kind=drill row links to the target
// it drills — the same back-link shape as a backup row, because a drill fires
// against the same target reference (A5).
func TestScheduleRowsLinkBackToDrillTarget(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "monthly-drill", CronExpr: "@every 30d", Kind: store.ScheduleDrill,
		Args: store.ScheduleArgs{Target: "nas01"},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/schedules")
	if code != http.StatusOK {
		t.Fatalf("GET /schedules = %d, want 200", code)
	}
	if !strings.Contains(page, `href="/system/backups" class="mono">nas01</a>`) {
		t.Errorf("/schedules drill row missing the back-link to its target:\n%s", page)
	}
}

// TestScheduleBackLinkResolves: the back-link's destination — /system/backups,
// the target's row on the backups management list — is a live route, so the
// link an operator follows lands instead of 404ing. There is no per-target
// history page yet (A4); /system/backups is the target's home for now.
func TestScheduleBackLinkResolves(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "nightly-backup", CronExpr: "@every 24h", Kind: store.ScheduleBackup,
		Args: store.ScheduleArgs{Target: "nas01"},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, _ := getBody(t, client, ts.URL+"/system/backups")
	if code != http.StatusOK {
		t.Fatalf("GET /system/backups = %d, want 200", code)
	}
}

// TestScheduleRowDoesNotLinkPlayInventory: a kind=play row's target is a
// standing inventory name, not a backup target — it renders the name plain
// and never links to /system/backups, so the back-link stays a backup/drill
// fact (the kind gate).
func TestScheduleRowDoesNotLinkPlayInventory(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := h.store.CreateSchedule(ctx, store.NewSchedule{
		Name: "converge", CronExpr: "@every 6h", Kind: "play",
		Args: store.ScheduleArgs{Argv: []string{"site.yml"}, Target: "fleet"},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)

	code, page := getBody(t, client, ts.URL+"/schedules")
	if code != http.StatusOK {
		t.Fatalf("GET /schedules = %d, want 200", code)
	}
	if !strings.Contains(page, "fleet") {
		t.Errorf("/schedules play row missing its inventory name:\n%s", page)
	}
	if strings.Contains(page, `href="/system/backups" class="mono">fleet</a>`) {
		t.Error("play row rendered a backup back-link for a standing inventory")
	}
}
