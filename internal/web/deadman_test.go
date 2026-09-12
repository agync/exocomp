package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/deadman"
	"exocomp.dev/exocomp/internal/store"
)

// seedBackup registers one enabled target and one run finishing at the given
// time in the given state, so the dead-man's states are deterministic.
func seedBackup(t *testing.T, h *harness, name, state string, finishedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	tgt, err := h.store.CreateBackupTarget(ctx, store.BackupTarget{
		Name: name, Kind: "restic_local", URI: "/srv/x", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	run, err := h.store.StartBackupRun(ctx, tgt.ID)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run.State = state
	run.FinishedAt = &finishedAt
	if state == store.BackupSucceeded {
		run.SnapshotID = "a3f9c2deadbeef"
	}
	if err := h.store.FinishBackupRun(ctx, run); err != nil {
		t.Fatalf("finish run: %v", err)
	}
}

// seedHeartbeat seals the dead-man heartbeat URL the way the Arm write does, so
// an ARMED/TRIPPED card test can draw the wired fact without going through a
// POST.
func seedHeartbeat(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	sealed, err := h.env.Seal(deadman.SecretHeartbeatURL, "url", []byte("https://93.184.216.34/abcdef"))
	if err != nil {
		t.Fatalf("seal heartbeat url: %v", err)
	}
	_, err = h.store.CreateSecret(ctx, store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		AAD: sealed.Name + "|" + sealed.Kind, Source: store.SecretSourceUser,
	})
	if err != nil && !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("create heartbeat secret: %v", err)
	}
}

// TestDeadmanNone is the GATE for the NONE state: no threshold renders the —
// figure, the "no threshold" meta, the last-good fact, and exactly one verb —
// Arm — the gap reading as the fix.
func TestDeadmanNone(t *testing.T) {
	h := newHarness(t)
	seedBackup(t, h, "nas01", store.BackupSucceeded, time.Now().Add(-16*time.Hour).UTC())
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		`>NONE<`,
		`<span class="value dim">—</span>`,
		"no threshold, no heartbeat · nothing trips if backups stop",
		"last good backup",
		"16h 00m ago · nas01 #a3f9c2",
		"not wired — no external heartbeat yet",
		`>Arm</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("NONE card missing %q", want)
		}
	}
	for _, absent := range []string{`>ARMED<`, `>TRIPPED<`, "until it trips", "since a backup succeeded"} {
		if strings.Contains(page, absent) {
			t.Errorf("NONE card shows %q", absent)
		}
	}
}

// TestDeadmanArmed is the GATE for the ARMED state: the time-left figure in the
// same slot, the threshold and heartbeat facts, and the pencil Arm verb plus ▾.
func TestDeadmanArmed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// 15h 30m ago → 32h 30m left of the 48h threshold, stable across seconds of
	// test drift.
	finished := time.Now().Add(-(15*time.Hour + 30*time.Minute)).UTC()
	seedBackup(t, h, "nas01", store.BackupSucceeded, finished)
	seedHeartbeat(t, h)
	if err := h.store.PutSetting(ctx, store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting threshold: %v", err)
	}
	if err := h.store.PutSetting(ctx, store.SettingDeadmanPingedAt,
		itoa64(finished.UnixMilli())); err != nil {
		t.Fatalf("PutSetting pinged_at: %v", err)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		`>ARMED<`,
		`>32h 2`,
		"until it trips",
		"48h without a succeeded backup",
		"wired · sealed deadman/heartbeat_url · pinged 15h 30m ago",
		`data-modal-open="deadman-arm"`,
		`title="Arm — change the threshold or heartbeat"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("ARMED card missing %q", want)
		}
	}
	if !strings.Contains(page, "Disarm") {
		t.Errorf("ARMED card missing the Disarm verb under the menu")
	}
}

// TestDeadmanTripped is the GATE for the TRIPPED state: the time-since figure,
// the newest-runs and paged facts, and the Queue backup primary naming the
// enabled target.
func TestDeadmanTripped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	finished := time.Now().Add(-(49*time.Hour + 2*time.Minute)).UTC()
	seedBackup(t, h, "nas01", store.BackupSucceeded, finished)
	seedBackup(t, h, "b2-offsite", store.BackupFailed, time.Now().Add(-61*time.Hour).UTC())
	seedHeartbeat(t, h)
	if err := h.store.PutSetting(ctx, store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting threshold: %v", err)
	}
	if err := h.store.PutSetting(ctx, store.SettingDeadmanPagedAt,
		itoa64(time.Now().Add(-(1*time.Hour + 2*time.Minute)).UTC().UnixMilli())); err != nil {
		t.Fatalf("PutSetting paged_at: %v", err)
	}
	if err := h.store.PutSetting(ctx, store.SettingDeadmanPingedAt,
		itoa64(finished.UnixMilli())); err != nil {
		t.Fatalf("PutSetting pinged_at: %v", err)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	for _, want := range []string{
		`>TRIPPED<`,
		`>49h 02m</span>`,
		"since a backup succeeded · threshold 48h",
		"nas01 succeeded 49h 02m ago · b2-offsite failed 61h 00m ago",
		"last ping 49h 02m ago · the external service is paging",
		`<dd><span class="paged-word">ntfy</span><span class="paged-facts"> · once · 1h 02m ago</span></dd>`,
		"Clears itself the moment any target&#39;s run SUCCEEDS. A failed or still-running newer run does not mask the age.",
		`>Queue backup — nas01</button>`,
		`action="/system/deadman/queue"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("TRIPPED card missing %q", want)
		}
	}
	// The armed figure slot is NOT the time-left form — the meaning changed,
	// not the slot.
	if strings.Contains(page, "until it trips") {
		t.Error("TRIPPED card shows the ARMED figure meta")
	}
}

// TestDeadmanArmSealsAndNeverEchoes is the GATE for the Arm write AND the
// never-echo rule (invariant 4): submitting the dialog sets the threshold,
// seals the heartbeat URL, and no render ever contains the value. The URL is a
// literal public IP so the registration-time SSRF gate accepts it without DNS.
func TestDeadmanArmSealsAndNeverEchoes(t *testing.T) {
	h := newHarness(t)
	// A recent success so the armed card renders the wired heartbeat fact (the
	// armed-with-no-history edge trips and reads the paging form instead).
	seedBackup(t, h, "nas01", store.BackupSucceeded, time.Now().Add(-time.Hour).UTC())
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	heartbeat := "https://93.184.216.34/abcdef"
	resp := postForm(t, client, ts.URL+"/system/deadman/arm",
		url.Values{"threshold": {"48h"}, "heartbeat_url": {heartbeat}, "from": {"/system"}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("arm = %d, want 303", resp.StatusCode)
	}

	ctx := context.Background()
	th, err := h.store.GetSetting(ctx, store.SettingDeadmanThreshold)
	if err != nil || th != "48h" {
		t.Fatalf("threshold = %q (err %v), want 48h", th, err)
	}
	if _, err := h.store.GetSecret(ctx, deadman.SecretHeartbeatURL); err != nil {
		t.Fatalf("heartbeat secret not sealed: %v", err)
	}

	// Never-echo: the value is absent from every surface that renders the card
	// or the dialog.
	for _, path := range []string{"/system", "/system/deadman"} {
		code, page := getBody(t, client, ts.URL+path)
		if code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, code)
		}
		if strings.Contains(page, heartbeat) {
			t.Errorf("%s echoes the heartbeat value", path)
		}
		if !strings.Contains(page, "wired · sealed deadman/heartbeat_url · pinged never") {
			t.Errorf("%s does not render the wired heartbeat fact", path)
		}
	}
}

// TestDeadmanArmThresholdOnly is the SPEC's optional half: the threshold alone
// arms the internal half, and the card reads the heartbeat as not wired.
func TestDeadmanArmThresholdOnly(t *testing.T) {
	h := newHarness(t)
	seedBackup(t, h, "nas01", store.BackupSucceeded, time.Now().Add(-16*time.Hour).UTC())
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/deadman/arm",
		url.Values{"threshold": {"72h"}, "from": {"/system"}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("arm = %d, want 303", resp.StatusCode)
	}
	ctx := context.Background()
	if th, _ := h.store.GetSetting(ctx, store.SettingDeadmanThreshold); th != "72h" {
		t.Fatalf("threshold = %q, want 72h", th)
	}
	if _, err := h.store.GetSecret(ctx, deadman.SecretHeartbeatURL); err == nil {
		t.Fatal("threshold-only arm must not seal a heartbeat URL")
	}
	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK || !strings.Contains(page, "not wired — no external heartbeat yet") {
		t.Errorf("threshold-only arm must read the heartbeat as not wired")
	}
}

// TestDeadmanArmRefusesBadThreshold pins the segmented switch's contract: only
// the offered values arm, and a 422 re-renders the page with the dialog open
// and the refusal on the threshold's meta line.
func TestDeadmanArmRefusesBadThreshold(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/deadman/arm",
		url.Values{"threshold": {"100h"}, "from": {"/system"}}, nil)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("arm = %d, want 422", resp.StatusCode)
	}
	for _, want := range []string{
		"Pick one of the offered thresholds — 24h, 48h, 72h or 7d.",
		`exoModalOpen("deadman-arm")`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("refusal page missing %q", want)
		}
	}
	ctx := context.Background()
	if _, err := h.store.GetSetting(ctx, store.SettingDeadmanThreshold); err == nil {
		t.Fatal("a refused arm must not write the threshold")
	}
}

// TestDeadmanDisarm is the GATE for the ▾ Disarm verb: it removes the threshold
// and the sealed heartbeat URL, returning the card to NONE.
func TestDeadmanDisarm(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedBackup(t, h, "nas01", store.BackupSucceeded, time.Now().Add(-16*time.Hour).UTC())
	seedHeartbeat(t, h)
	if err := h.store.PutSetting(ctx, store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting threshold: %v", err)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK || !strings.Contains(page, `>ARMED<`) {
		t.Fatal("fixture must start armed")
	}
	resp := postForm(t, client, ts.URL+"/system/deadman/disarm",
		url.Values{"from": {"/system"}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("disarm = %d, want 303", resp.StatusCode)
	}
	if _, err := h.store.GetSetting(ctx, store.SettingDeadmanThreshold); err == nil {
		t.Fatal("disarm must remove the threshold")
	}
	if _, err := h.store.GetSecret(ctx, deadman.SecretHeartbeatURL); err == nil {
		t.Fatal("disarm must remove the sealed heartbeat URL")
	}
	code, page = getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK || !strings.Contains(page, `>NONE<`) {
		t.Errorf("after disarm the card must read NONE")
	}
}

// TestDeadmanQueueBackup is the GATE for the TRIPPED primary: Queue backup
// enqueues one kind-backup job for the target through the one queue, attributed
// to the operator.
func TestDeadmanQueueBackup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	finished := time.Now().Add(-49 * time.Hour).UTC()
	seedBackup(t, h, "nas01", store.BackupSucceeded, finished)
	if err := h.store.PutSetting(ctx, store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting threshold: %v", err)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	resp := postForm(t, client, ts.URL+"/system/deadman/queue",
		url.Values{"target": {"nas01"}, "from": {"/system"}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("queue = %d, want 303", resp.StatusCode)
	}
	jobs, err := h.store.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	found := false
	for _, j := range jobs {
		if j.Kind != backup.JobKind || j.Runbook != "backup" {
			continue
		}
		tgt, terr := backup.TargetOf(j.Args.Argv)
		if terr == nil && tgt == "nas01" {
			found = true
		}
	}
	if !found {
		t.Fatal("no kind-backup job for nas01 was enqueued")
	}
}

// TestDeadmanWriteRefusedForReadonly pins the write floor: a readonly token is
// refused the Arm write, and nothing is sealed or set.
func TestDeadmanWriteRefusedForReadonly(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "ro", "readonly"))

	resp := postForm(t, client, ts.URL+"/system/deadman/arm",
		url.Values{"threshold": {"48h"}, "from": {"/system"}}, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("arm with readonly = %d, want 403", resp.StatusCode)
	}
	if _, err := h.store.GetSetting(context.Background(), store.SettingDeadmanThreshold); err == nil {
		t.Fatal("readonly arm must not write the threshold")
	}
}

// TestDeadmanLegPage renders /system/deadman (the phone rows' land target):
// the full-width card with its verbs, 200 for a signed-in viewer.
func TestDeadmanLegPage(t *testing.T) {
	h := newHarness(t)
	// An armed card so the page shows the Disarm verb the phone flow relies on.
	seedBackup(t, h, "nas01", store.BackupSucceeded, time.Now().Add(-time.Hour).UTC())
	if err := h.store.PutSetting(context.Background(), store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting threshold: %v", err)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system/deadman")
	if code != http.StatusOK {
		t.Fatalf("/system/deadman = %d, want 200", code)
	}
	for _, want := range []string{
		`<section class="leg" id="deadman"`,
		`action="/system/deadman/arm"`,
		`action="/system/deadman/disarm"`,
		`aria-label="Dead-man"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("leg page missing %q", want)
		}
	}
}

// TestDeadmanPowerlineReadsOneRule pins the round's hard rule at the web face:
// the dead-man's powerline segment quotes the store's ONE succeeded-age rule —
// a newer FAILED run never masks the older good backup — and carries the card's
// state colour without recomputing the age.
func TestDeadmanPowerlineReadsOneRule(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedBackup(t, h, "nas01", store.BackupSucceeded, time.Now().Add(-16*time.Hour).UTC())
	seedBackup(t, h, "b2-offsite", store.BackupFailed, time.Now().Add(-30*time.Minute).UTC())
	if err := h.store.PutSetting(ctx, store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting threshold: %v", err)
	}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	client := login(t, ts.URL, h.mint(t, "laptop", "operator"))

	code, page := getBody(t, client, ts.URL+"/system")
	if code != http.StatusOK {
		t.Fatalf("/system = %d, want 200", code)
	}
	// The failed run finished 30m ago but the segment reads the succeeded
	// run's 16h age — the ONE rule, never the newest row.
	if !strings.Contains(page, `dead-man 16h`) {
		t.Errorf("powerline does not read the succeeded run's age")
	}
	if strings.Contains(page, `dead-man 30m`) {
		t.Errorf("powerline reads the failed run's age")
	}
	// The segment carries the card's chip colour (armed → ok) from the same
	// state the card draws, via the layout pl-seg's state class.
	if !strings.Contains(page, `class="pl-seg ok"`) {
		t.Errorf("powerline dead-man segment missing the armed state colour")
	}
}
