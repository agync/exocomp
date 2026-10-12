package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// TestTimezoneSupportAndPreferences verifies that timestamps are rendered with
// <time datetime="..."> tags and data-time-format attributes, and that the /system
// page contains the Time display preferences control allowing switching between
// viewer local timezone and UTC.
func TestTimezoneSupportAndPreferences(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	ctx := context.Background()
	now := time.Date(2026, 10, 12, 19, 0, 0, 0, time.UTC)
	h.server.clock = func() time.Time { return now }

	// Seed a completed job
	job, err := h.store.CreateJob(ctx, store.NewJob{
		Runbook: "deploy", Kind: "playbook", Trigger: "manual",
		Args: store.JobArgs{Argv: []string{"ansible-playbook", "deploy.yml"}},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateQueued, store.StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := h.store.TransitionJob(ctx, job.ID, store.StateRunning, store.StateSucceeded); err != nil {
		t.Fatal(err)
	}
	updated, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.FinishedAt == nil {
		t.Fatal("finished job missing FinishedAt")
	}

	// 1. Check layout includes timeInit and exoSyncTimes
	_, body := getBody(t, client, ts.URL+"/jobs")
	if !strings.Contains(body, "exoSyncTimes") {
		t.Error("page missing exoSyncTimes client script")
	}
	if !strings.Contains(body, "exoSetTimezone") {
		t.Error("page missing exoSetTimezone client script")
	}

	// 2. Check /jobs renders time tag with datetime and data-time-format
	if !strings.Contains(body, `<time datetime="`+updated.FinishedAt.UTC().Format("2006-01-02T15:04:05Z07:00")+`" data-time-format="rel">`) {
		t.Errorf("jobs list missing <time> tag for finished run, got:\n%s", body)
	}

	// 3. Check /system page renders display preferences with Local and UTC options
	_, sysBody := getBody(t, client, ts.URL+"/system")
	if !strings.Contains(sysBody, `id="sys-prefs"`) {
		t.Errorf("/system missing sys-prefs preferences card, got:\n%s", sysBody)
	}
	if !strings.Contains(sysBody, `id="tz-btn-local"`) || !strings.Contains(sysBody, `id="tz-btn-utc"`) {
		t.Errorf("/system missing tz-btn-local or tz-btn-utc toggle buttons, got:\n%s", sysBody)
	}
	if !strings.Contains(sysBody, `onclick="exoSetTimezone('local')"`) || !strings.Contains(sysBody, `onclick="exoSetTimezone('utc')"`) {
		t.Error("/system toggle buttons missing exoSetTimezone onclick handlers")
	}
}
