package deadman

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// capture is a notify.Transport that records every notification it is handed,
// so a test can assert on what the monitor pushed.
type capture struct {
	mu    sync.Mutex
	notes []notify.Notification
}

func (c *capture) Name() string { return "capture" }
func (c *capture) Publish(_ context.Context, n notify.Notification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notes = append(c.notes, n)
	return nil
}

func (c *capture) got() []notify.Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]notify.Notification(nil), c.notes...)
}

// fakeClock is a mutable clock the monitor reads, so a test can move time and
// watch a trip form, clear, and re-form.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedRun registers an enabled target and one run finishing at the given time
// in the given state, the store's dead-man fixture shape.
func seedRun(t *testing.T, st *store.Store, targetName, state string, finishedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	tgt, err := st.CreateBackupTarget(ctx, store.BackupTarget{
		Name: targetName, Kind: "restic_local", URI: "/srv/x", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	run, err := st.StartBackupRun(ctx, tgt.ID)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run.State = state
	run.FinishedAt = &finishedAt
	if state == store.BackupSucceeded {
		run.SnapshotID = "snap"
	}
	if err := st.FinishBackupRun(ctx, run); err != nil {
		t.Fatalf("finish run: %v", err)
	}
}

// newMonitor builds a monitor over the given store, clock, and optional
// heartbeat server (nil means no heartbeat URL is wired). It returns the
// capture transport so the test can assert on pushes.
func newMonitor(t *testing.T, st *store.Store, clk *fakeClock, hb *httptest.Server) (*Monitor, *capture) {
	t.Helper()
	capGot := &capture{}
	notifier := notify.New(slog.New(slog.NewTextHandler(io.Discard, nil)), capGot)
	var look notify.SecretLookup
	if hb != nil {
		url := hb.URL
		look = func(_ context.Context, name string) ([]byte, error) {
			if name != SecretHeartbeatURL {
				return nil, errors.New("no such secret")
			}
			return []byte(url), nil
		}
	}
	m := NewMonitor(Options{
		Store: st, Look: look, Notifier: notifier, Host: "nibbler", Clock: clk.now,
		// A plain client so the ping test reaches the local httptest server: the
		// SSRF dial guard is notify's own tested surface (unifiedpush_test.go
		// uses the same plain-transport shape), and production keeps the default
		// GuardedClient.
		Client: &http.Client{},
	})
	return m, capGot
}

// TestMonitorPagesOnce is the GATE for "ntfy pages once": an armed dead-man
// whose age has blown the threshold pushes dead-man tripped HIGH on the first
// tick and stays silent on every later tick until a run succeeds.
func TestMonitorPagesOnce(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	clk := &fakeClock{t: base}
	seedRun(t, st, "nas01", store.BackupSucceeded, base.Add(-50*time.Hour))
	if err := st.PutSetting(ctx, store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting threshold: %v", err)
	}

	m, capGot := newMonitor(t, st, clk, nil)
	m.Tick(ctx)
	m.Tick(ctx)
	m.Tick(ctx)

	got := capGot.got()
	if len(got) != 1 {
		t.Fatalf("paged %d times, want once", len(got))
	}
	n := got[0]
	if n.Title != "dead-man tripped" {
		t.Errorf("title = %q, want dead-man tripped", n.Title)
	}
	if n.Priority != notify.PriorityHigh {
		t.Errorf("priority = %v, want high", n.Priority)
	}
	if n.Source != "deadman" {
		t.Errorf("source = %q, want deadman (so a scoped mute can hold it)", n.Source)
	}
	if want := "50h 00m since a backup succeeded on nibbler"; n.Body != want {
		t.Errorf("body = %q, want %q", n.Body, want)
	}
	// The once-marker persists so a daemon restart cannot re-page the trip.
	paged, err := st.GetSetting(ctx, store.SettingDeadmanPagedAt)
	if err != nil {
		t.Fatalf("GetSetting paged_at: %v", err)
	}
	if paged != itoa(base.UnixMilli()) {
		t.Errorf("paged_at = %q, want %d", paged, base.UnixMilli())
	}
}

// TestMonitorRearmsOnNextSuccess is the GATE for "re-arm on the next SUCCEEDED
// run": a succeeded run pings the heartbeat, clears the once-marker, and the
// dead-man trips again — paging once more — the next time the threshold blows.
func TestMonitorRearmsOnNextSuccess(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	clk := &fakeClock{t: base}
	seedRun(t, st, "nas01", store.BackupSucceeded, base.Add(-50*time.Hour))
	if err := st.PutSetting(ctx, store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting threshold: %v", err)
	}

	var hits int32
	hb := newHeartbeatServer(&hits)
	defer hb.Close()

	m, capGot := newMonitor(t, st, clk, hb)
	m.Tick(ctx) // trips and pages once
	if n := len(capGot.got()); n != 1 {
		t.Fatalf("paged %d times, want 1 after the first trip", n)
	}

	// A newer success clears the trip: ping fires, the marker is gone, and the
	// dead-man stops paging.
	clk.advance(time.Hour)
	seedRun(t, st, "nas02", store.BackupSucceeded, clk.now().Add(-time.Hour))
	m.Tick(ctx)
	m.Tick(ctx)
	if n := len(capGot.got()); n != 1 {
		t.Fatalf("paged %d times after a success, want still 1", n)
	}
	if hits != 1 {
		t.Errorf("heartbeat pinged %d times, want 1", hits)
	}
	if _, err := st.GetSetting(ctx, store.SettingDeadmanPagedAt); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("paged_at after success = %v, want cleared (ErrNotFound)", err)
	}
	pinged, err := st.GetSetting(ctx, store.SettingDeadmanPingedAt)
	if err != nil {
		t.Fatalf("GetSetting pinged_at: %v", err)
	}
	if pinged != itoa(clk.now().UnixMilli()) {
		t.Errorf("pinged_at = %q, want %d", pinged, clk.now().UnixMilli())
	}

	// Time moves on; the good backup ages past the threshold again and a fresh
	// trip pages once more.
	clk.advance(48 * time.Hour)
	m.Tick(ctx)
	m.Tick(ctx)
	if n := len(capGot.got()); n != 2 {
		t.Fatalf("paged %d times after the second trip, want 2", n)
	}
}

// TestMonitorNoThresholdNothingTrips pins the NONE state's contract: without a
// threshold, no amount of backup age pages anything.
func TestMonitorNoThresholdNothingTrips(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	clk := &fakeClock{t: base}
	seedRun(t, st, "nas01", store.BackupSucceeded, base.Add(-72*time.Hour))

	m, capGot := newMonitor(t, st, clk, nil)
	m.Tick(ctx)
	m.Tick(ctx)
	if n := len(capGot.got()); n != 0 {
		t.Fatalf("paged %d times with no threshold, want 0", n)
	}
}

// TestMonitorArmedNeverSucceededTrips pins the armed-with-no-history edge: a
// threshold with no succeeded run ever is unbounded age, so it trips (and the
// page says so plainly rather than inventing a time-since).
func TestMonitorArmedNeverSucceededTrips(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	clk := &fakeClock{t: time.Now().UTC().Truncate(time.Second)}
	if err := st.PutSetting(ctx, store.SettingDeadmanThreshold, "48h"); err != nil {
		t.Fatalf("PutSetting threshold: %v", err)
	}

	m, capGot := newMonitor(t, st, clk, nil)
	m.Tick(ctx)
	got := capGot.got()
	if len(got) != 1 {
		t.Fatalf("paged %d times, want once", len(got))
	}
	if want := "no backup has ever succeeded on nibbler"; got[0].Body != want {
		t.Errorf("body = %q, want %q", got[0].Body, want)
	}
}

// TestCountdownAge pins the figure rendering the dead-man quotes everywhere:
// hours and minutes while the leading unit matters, days past a week.
func TestCountdownAge(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{31*time.Hour + 12*time.Minute, "31h 12m"},
		{49*time.Hour + 2*time.Minute, "49h 02m"},
		{2 * time.Hour, "2h 00m"},
		{3 * time.Minute, "3m"},
		{7*24*time.Hour + 2*time.Hour, "7d 02h"},
		{6*24*time.Hour + 3*time.Hour, "147h 00m"},
		{30 * time.Second, "0m"},
		{-time.Hour, "0m"},
	} {
		if got := CountdownAge(tc.d); got != tc.want {
			t.Errorf("CountdownAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// newHeartbeatServer is an httptest server that counts every POST it receives,
// the stand-in for the external heartbeat service the sealed URL points at.
func newHeartbeatServer(hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
}
