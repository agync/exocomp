// Package deadman is the dead-man leg's daemon side: the trip monitor that
// compares the ONE backup-age rule (store.LastSucceededBackupTime) to the
// operator's threshold, pages the notify fan-out once when it trips, and pings
// the sealed heartbeat URL on every new succeeded backup. The web surface
// renders the state (internal/web) and writes the settings/secrets; only the
// daemon runs the monitor, because only the daemon holds the notifier (the
// daemon-seam pattern, cf. internal/web/api_maintenance.go). Nothing here
// rebuilds the notify fan-out — it hands notifications to the notifier the
// daemon already built.
package deadman

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// SecretHeartbeatURL is the sealed secret the external heartbeat service URL
// is stored under. It is sealed at Arm time and never echoed (invariant 4);
// the card reads its presence, and the monitor reads the plaintext only to
// ping it.
const SecretHeartbeatURL = "deadman/heartbeat_url"

// CountdownAge renders a duration the way the dead-man's figure and push body
// read it: "31h 12m" / "49h 02m" while hours lead, "7d 02h" past a week — the
// countdownFace the SPEC defaults to, finer than the surfaces' humanAge (which
// rounds to one unit), because the figure is the one number the operator came
// for.
func CountdownAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	if h >= 24*7 {
		return fmt.Sprintf("%dd %02dh", h/24, h%24)
	}
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// Monitor is the running daemon's dead-man watch. It is a clock-driven loop
// (Run) that reads only the store and the notifier the daemon built; the web
// surface never sees it.
type Monitor struct {
	store    *store.Store
	look     notify.SecretLookup
	notifier *notify.Notifier
	host     string
	log      *slog.Logger
	clock    func() time.Time
	client   *http.Client
	// lastSuccess is the newest succeeded backup time the monitor has seen. A
	// newer value on a tick means a backup succeeded since the last tick, which
	// pings the heartbeat and re-arms the once-page marker.
	lastSuccess time.Time
}

// Options configures a Monitor. Look is the daemon's sealed-secret reader (the
// same look its transports load through); Client dials the heartbeat URL and
// defaults to notify.GuardedClient, the SSRF-guarded client every
// client-supplied endpoint dials through.
type Options struct {
	Store    *store.Store
	Look     notify.SecretLookup
	Notifier *notify.Notifier
	Host     string
	Logger   *slog.Logger
	Clock    func() time.Time
	Client   *http.Client
}

// NewMonitor returns a Monitor over the daemon's store and notifier.
func NewMonitor(opts Options) *Monitor {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	client := opts.Client
	if client == nil {
		client = notify.GuardedClient()
	}
	return &Monitor{
		store: opts.Store, look: opts.Look, notifier: opts.Notifier,
		host: opts.Host, log: log, clock: clock, client: client,
	}
}

// Run ticks the dead-man logic on the interval until ctx is cancelled. The
// newest success is primed on entry so a restart does not treat the pre-existing
// good backup as a fresh one demanding a ping.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	if when, ok := m.store.LastSucceededBackupTime(ctx); ok {
		m.lastSuccess = when
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.Tick(ctx)
	}
}

// Tick runs one pass of the dead-man watch. Exported for tests; Run calls it on
// its interval.
func (m *Monitor) Tick(ctx context.Context) {
	now := m.clock().UTC()

	success, ok := m.store.LastSucceededBackupTime(ctx)
	if ok {
		if !m.lastSuccess.IsZero() && success.After(m.lastSuccess) {
			m.onSuccess(ctx, now)
		}
		m.lastSuccess = success
	} else {
		m.lastSuccess = time.Time{}
	}

	raw, err := m.store.GetSetting(ctx, store.SettingDeadmanThreshold)
	if err != nil || strings.TrimSpace(raw) == "" {
		return // NONE: no threshold, nothing trips.
	}
	threshold, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return // A broken threshold is the operator's to fix; paging would be noise.
	}
	if !ok {
		// Armed but nothing has ever succeeded: the age is unbounded, so the
		// dead-man has blown the threshold.
		m.pageOnce(ctx, now, false, 0)
		return
	}
	if age := now.Sub(success); age >= threshold {
		m.pageOnce(ctx, now, true, age)
	}
}

// onSuccess runs the one thing a succeeded backup owes the dead-man: the
// heartbeat ping, and the re-arm that clears the once-page marker so a later
// trip pages again.
func (m *Monitor) onSuccess(ctx context.Context, now time.Time) {
	if err := m.store.DeleteSetting(ctx, store.SettingDeadmanPagedAt); err != nil {
		m.log.Error("deadman: clear page marker", "err", err)
	}
	m.ping(ctx, now)
}

// pageOnce pushes "dead-man tripped" exactly once per trip. The marker is
// written regardless of delivery: the fan-out logs a failed transport, and
// once-per-trip is the contract — a redelivery loop would only add noise to the
// exact alarm that is supposed to be loud once.
func (m *Monitor) pageOnce(ctx context.Context, now time.Time, ok bool, age time.Duration) {
	if _, err := m.store.GetSetting(ctx, store.SettingDeadmanPagedAt); err == nil {
		return // Already paged this trip.
	}
	if m.notifier != nil {
		body := "no backup has ever succeeded on " + m.host
		if ok {
			body = CountdownAge(age) + " since a backup succeeded on " + m.host
		}
		_ = m.notifier.Notify(ctx, notify.Notification{
			Title:    "dead-man tripped",
			Body:     body,
			Priority: notify.PriorityHigh,
			Tags:     []string{"rotating_light", "kind:deadman"},
			Click:    "exocomp://system/deadman",
			Source:   "deadman",
			Host:     m.host,
		})
	}
	if err := m.store.PutSetting(ctx, store.SettingDeadmanPagedAt,
		strconv.FormatInt(now.UnixMilli(), 10)); err != nil {
		m.log.Error("deadman: record page", "err", err)
	}
}

// ping posts one heartbeat to the sealed URL. A transport error is logged and
// never fatal — the external service's whole purpose is to page when pings
// stop, so a single failed ping is not something exocomp decides about. The
// pinged_at marker is stamped only on a usable answer, so the card's "pinged
// {age} ago" is never a lie.
func (m *Monitor) ping(ctx context.Context, now time.Time) {
	if m.look == nil {
		return
	}
	raw, err := m.look(ctx, SecretHeartbeatURL)
	if err != nil {
		return // Not wired.
	}
	target := strings.TrimSpace(string(raw))
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		m.log.Warn("deadman: heartbeat URL unusable", "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target,
		bytes.NewReader([]byte("exocomp heartbeat")))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := m.client.Do(req)
	if err != nil {
		m.log.Warn("deadman: heartbeat ping failed", "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		m.log.Warn("deadman: heartbeat answered", "status", resp.Status)
		return
	}
	if err := m.store.PutSetting(ctx, store.SettingDeadmanPingedAt,
		strconv.FormatInt(now.UnixMilli(), 10)); err != nil {
		m.log.Error("deadman: record ping", "err", err)
	}
}
