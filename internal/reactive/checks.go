package reactive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// DefaultHysteresis is how many consecutive observations a status must hold
// before it becomes a declared transition and an event is emitted, in both
// directions. A target on marginal wifi cannot otherwise flap up/down/up —
// each flap would withdraw and re-raise a pending approval, churning the
// events table.
const DefaultHysteresis = 3

// obsRingLen bounds a target's observation ring: a little slack over the
// hysteresis window so a transition's last-3 observations are always in hand at
// emission, without the ring growing without bound.
const obsRingLen = 5

// probeTimeout bounds one probe: a dial, an http GET, or a ping subprocess, so
// a wedged target reports down instead of hanging the sweep.
const probeTimeout = 5 * time.Second

// checkDedupKey names one target of one check. It is the condition the
// pipeline dedups on and a check-source rule's per-target cooldown keys off,
// so one target of one check is one slot.
func checkDedupKey(checkName, targetName string) string {
	return SourceCheck + ":" + checkName + ":" + targetName
}

// CheckObservation is one probe of the hysteresis ledger: what was observed and
// the consecutive count it advanced. Values-free by construction (timestamp,
// status, count) — it is embedded into the event a transition raises.
type CheckObservation struct {
	ObservedAt  string `json:"observed_at"`
	Status      string `json:"status"`
	Consecutive int    `json:"consecutive"`
}

// appendObservation adds one probe to a target's observation ring (JSON,
// newest last) and trims to obsRingLen. The ring is stored whole and read whole
// at emission; it is never queried by column.
func appendObservation(ring string, status string, consecutive int, at time.Time) string {
	obs := make([]CheckObservation, 0, obsRingLen)
	_ = json.Unmarshal([]byte(ring), &obs) // a corrupt ring degrades to a fresh one
	obs = append(obs, CheckObservation{
		ObservedAt: at.UTC().Format(time.RFC3339), Status: status, Consecutive: consecutive,
	})
	if len(obs) > obsRingLen {
		obs = obs[len(obs)-obsRingLen:]
	}
	b, err := json.Marshal(obs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// transitionObservations is the tail of the ring that produced a transition: the
// observations of the transition status, kept to the hysteresis window, newest
// last. The ring can hold both statuses after a flip, so it is filtered — the
// "WHY THIS COUNTED" ledger is homogeneous (`observed up · 1 of 3 … 3 of 3`),
// never a mix.
func transitionObservations(ring, status string) []CheckObservation {
	var obs []CheckObservation
	if err := json.Unmarshal([]byte(ring), &obs); err != nil {
		return nil
	}
	out := make([]CheckObservation, 0, DefaultHysteresis)
	for _, o := range obs {
		if o.Status != status {
			continue
		}
		out = append(out, o)
	}
	if len(out) > DefaultHysteresis {
		out = out[len(out)-DefaultHysteresis:]
	}
	return out
}

// SweepChecks probes every enabled check that is due and emits one event per
// declared transition, into the same pipeline every webhook goes through. It
// returns how many events were ingested.
//
// The due map is process state, like the daily-cap page ledger: the first call
// probes everything (cold-start seeding makes that safe) and a daemon restart
// re-seeds it, so the first sweep after a restart probes all targets again —
// which the seed absorbs without emitting. The event table is the durable
// checkpoint: an event's dedup key names check+target, so a probe that got
// ingested survives a restart whether or not the in-memory due map did.
func (e *Engine) SweepChecks(ctx context.Context) (int, error) {
	checks, err := e.store.ListChecks(ctx, true)
	if err != nil {
		return 0, err // structural: the sweep cannot run at all
	}
	now := e.now()

	raised := 0
	for _, c := range checks {
		if next, ok := e.checkDue(c.ID); ok && now.Before(next) {
			continue
		}
		n, keep := e.sweepStep(ctx, "sweep checks", c.Name, func() (int, error) {
			return e.sweepCheck(ctx, c, now)
		})
		if !keep {
			return raised, ctx.Err()
		}
		raised += n
		// Marked due even when the check had a problem, so a failing check
		// keeps its interval cadence instead of re-probing every tick. The
		// sweep tick floors the effective interval at its own period (a 5s
		// check on a 30s tick probes every 30s).
		e.markCheckDue(c.ID, now.Add(time.Duration(c.IntervalS)*time.Second))
	}
	return raised, nil
}

// checkDue returns when a check is next due and whether it has been probed
// before. A check never probed is due now, which is what makes the first sweep
// probe everything — cold-start seeding absorbs the extra probes.
func (e *Engine) checkDue(id int64) (time.Time, bool) {
	e.dueMu.Lock()
	defer e.dueMu.Unlock()
	if e.dueChecks == nil {
		e.dueChecks = make(map[int64]time.Time)
		return time.Time{}, false
	}
	next, ok := e.dueChecks[id]
	return next, ok
}

func (e *Engine) markCheckDue(id int64, next time.Time) {
	e.dueMu.Lock()
	defer e.dueMu.Unlock()
	if e.dueChecks == nil {
		e.dueChecks = make(map[int64]time.Time)
	}
	e.dueChecks[id] = next
}

// sweepCheck probes every target of one check and updates check_state against
// each observation: a missing row is seeded without emitting, a steady state
// only advances the hysteresis counter, and a declared transition becomes one
// event. A broken target is logged and skipped, so one bad target cannot starve
// the check's other targets; only a cancelled context aborts the sweep.
func (e *Engine) sweepCheck(ctx context.Context, c store.Check, now time.Time) (int, error) {
	raised := 0
	for _, t := range c.Targets {
		if err := ctx.Err(); err != nil {
			return raised, err
		}
		up, dur, err := e.probe(ctx, c, t)
		if err != nil {
			e.log.Error("reactive: sweep checks: probe", "check", c.Name, "target", t.Name, "err", err)
			continue
		}
		status := checkStatus(up)

		// The classification is observation output, like the status: resolve it
		// once per probe and persist it on the state row, so every read path
		// reads the stored value instead of classifying again.
		labels, err := e.checkLabels(ctx, c, t)
		if err != nil {
			e.log.Error("reactive: sweep checks: classify", "check", c.Name, "target", t.Name, "err", err)
			continue
		}
		netgroup := labels["netgroup"]

		st, err := e.store.GetCheckState(ctx, c.ID, t.Name)
		if errors.Is(err, store.ErrNotFound) {
			// Cold-start seed: observe and record without emitting. emitted is
			// seeded to the observed result, never '' — with a '' seed the
			// `emitted != status` transition condition would be true on every
			// steady observation, so a daemon restart would spurious-fire
			// every up target (design §5.2). The seeded observation opens the
			// ring.
			if err := e.store.SetCheckState(ctx, store.CheckState{
				CheckID:            c.ID,
				TargetName:         t.Name,
				Status:             status,
				Consecutive:        1,
				Emitted:            status,
				FirstSeen:          now,
				LastChange:         now,
				RecentObservations: appendObservation("", status, 1, now),
				Netgroup:           netgroup,
				LastDurationMS:     dur.Milliseconds(),
			}); err != nil {
				e.log.Error("reactive: sweep checks: seed state", "check", c.Name, "target", t.Name, "err", err)
				continue
			}
			continue
		}
		if err != nil {
			e.log.Error("reactive: sweep checks: get state", "check", c.Name, "target", t.Name, "err", err)
			continue
		}

		if status == st.Status {
			st.Consecutive++
			if st.Consecutive > DefaultHysteresis {
				st.Consecutive = DefaultHysteresis
			}
		} else {
			st.Status = status
			st.Consecutive = 1
		}
		st.RecentObservations = appendObservation(st.RecentObservations, status, st.Consecutive, now)
		st.Netgroup = netgroup
		st.LastDurationMS = dur.Milliseconds()

		if st.Consecutive >= DefaultHysteresis && st.Emitted != st.Status {
			if err := e.emitCheckTransition(ctx, c, t, st, labels, now); err != nil {
				// The emitted flag is not advanced, so the next sweep
				// re-observes the same status and re-emits the transition.
				// Dedup-window-bounded and eventually consistent, not a
				// silent loss.
				e.log.Error("reactive: sweep checks: emit transition", "check", c.Name, "target", t.Name, "err", err)
				continue
			}
			st.Emitted = st.Status
			st.LastChange = now
			raised++
		}

		if err := e.store.SetCheckState(ctx, st); err != nil {
			e.log.Error("reactive: sweep checks: set state", "check", c.Name, "target", t.Name, "err", err)
			continue
		}
	}
	return raised, ctx.Err() // nil when healthy
}

// emitCheckTransition builds and ingests the one event a declared transition
// produces. The status here is the check_state vocabulary (up/down); the event
// maps it onto firing/resolved so a resolve finds and withdraws what its
// firing raised. labels are the target's already-resolved event labels, so the
// transition's netgroup is the same value the state row now carries.
func (e *Engine) emitCheckTransition(ctx context.Context, c store.Check, t store.CheckTarget, st store.CheckState, labels map[string]string, now time.Time) error {
	key := checkDedupKey(c.Name, t.Name)

	eventStatus := StatusFiring
	if st.Status == store.CheckDown {
		eventStatus = StatusResolved
	}

	// The hysteresis ledger the "WHY THIS COUNTED" card renders: the target's
	// observations of the transition status, kept to the window, embedded here
	// so the event stays self-sufficient after the ring moves on.
	var ledger []byte
	if obs := transitionObservations(st.RecentObservations, st.Status); len(obs) > 0 {
		if b, merr := json.Marshal(obs); merr == nil {
			ledger = b
		}
	}

	ev := Event{
		Source:            SourceCheck,
		Kind:              "check",
		Name:              c.Name,
		Status:            eventStatus,
		Severity:          "warning",
		Host:              t.Name,
		Fingerprint:       key,
		Summary:           fmt.Sprintf("check %s target %s is %s", c.Name, t.Name, st.Status),
		Labels:            labels,
		Annotations:       map[string]string{},
		ReceivedAt:        now,
		DedupKey:          key,
		CheckObservations: string(ledger),
	}

	address := t.Address
	if address == "" {
		address = t.URL
	}
	raw, err := json.Marshal(map[string]any{
		"check": c.Name, "target": t.Name, "kind": c.Kind, "address": address,
	})
	if err != nil {
		return err
	}
	_, err = e.Ingest(ctx, ev, raw)
	return err
}

// netgroupFor names the netgroup most specifically containing addr, or "away"
// when no registered group covers it. It is the one miss fallback the
// label-building paths share; peer.go's stamp keeps its own all-errors→"away"
// swallow on top because a webhook must never be refused for an unreadable
// netgroup table.
func (e *Engine) netgroupFor(ctx context.Context, addr netip.Addr) (string, error) {
	group, err := e.store.NetgroupForAddr(ctx, addr)
	if errors.Is(err, store.ErrNotFound) {
		return "away", nil
	}
	if err != nil {
		return "", err
	}
	return group, nil
}

// checkLabels builds the label set a check transition event carries. An http
// target (a URL, no Address) gets no netgroup label: its liveness is not a
// location verdict (the URL resolves off-LAN). An address target is classified
// against the netgroups, "away" when none matches.
func (e *Engine) checkLabels(ctx context.Context, c store.Check, t store.CheckTarget) (map[string]string, error) {
	labels := map[string]string{"target": t.Name}
	if t.Address == "" {
		return labels, nil
	}
	addr, err := netip.ParseAddr(t.Address)
	if err != nil {
		return nil, fmt.Errorf("sweep checks %s: target %q address %q does not parse: %w",
			c.Name, t.Name, t.Address, err)
	}
	group, err := e.netgroupFor(ctx, addr)
	if err != nil {
		return nil, err
	}
	labels["netgroup"] = group
	return labels, nil
}

// probe asks one target once and returns whether it answered and how long the
// probe took. Results are dial/exit truth only, never parsed output (AGENTS.md
// 3). A probe that cannot even start — an unparseable address or URL — is an
// error, not a down, so a misconfigured check surfaces in the sweep warning
// rather than reporting silently all-down.
func (e *Engine) probe(ctx context.Context, c store.Check, t store.CheckTarget) (up bool, dur time.Duration, err error) {
	start := time.Now()
	switch c.Kind {
	case store.CheckTCP:
		up, err = probeTCP(ctx, t)
	case store.CheckPing:
		up, err = probePing(ctx, t)
	case store.CheckHTTP:
		up, err = probeHTTP(ctx, t)
	default:
		err = fmt.Errorf("sweep checks %s: unknown kind %q", c.Name, c.Kind)
	}
	return up, time.Since(start), err
}

// probeTCP dials the target. Any dial failure — refused, timeout, unroutable —
// is a down; the address is the location, and an unreachable location answers
// nothing.
func probeTCP(ctx context.Context, t store.CheckTarget) (bool, error) {
	d := net.Dialer{Timeout: probeTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(t.Address, strconv.Itoa(t.Port)))
	if err != nil {
		return false, nil
	}
	_ = conn.Close()
	return true, nil
}

// probeHTTPClient builds the HTTP client the probe GETs through. It is a func
// so a test can substitute a transport that reaches a loopback test server
// without weakening the shipped dial-time guard (the same testability idiom as
// notify's lookupIP).
var probeHTTPClient = func() *http.Client {
	return &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: probeTimeout, Control: probeHTTPDialControl}).DialContext,
		},
	}
}

// probeHTTPDialControl is the net.Dialer.Control hook the http probe's client
// dials through, mirroring the push transport's dial-time SSRF check
// (notify/ssrf.go dialControl). It refuses the addresses a check must not
// reach — the daemon's own loopback, the cloud metadata service (link-local),
// and unspecified or multicast — at the concrete post-DNS IP, so a re-pointed
// name is blocked too. Unlike the push guard it keeps RFC1918 and ULA LAN
// targets allowed: probing the LAN is the point of a check. The metadata
// address 169.254.169.254 is link-local, so IsLinkLocalUnicast refuses it
// without naming it.
func probeHTTPDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("checks: http: refusing malformed dial address %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("checks: http: dial address %q is not a resolved IP", host)
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("checks: http: refusing %s: not a reachable LAN address", ip)
	}
	return nil
}

// probeHTTP GETs the URL and reads the status line only. The body is closed
// without being read: a health endpoint's response is not evidence this code
// should persist (AGENTS.md 4). Expect 0 means any 2xx/3xx answers up;
// otherwise the status must match exactly.
func probeHTTP(ctx context.Context, t store.CheckTarget) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return false, err
	}
	resp, err := probeHTTPClient().Do(req)
	if err != nil {
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if t.Expect == 0 {
		return resp.StatusCode >= 200 && resp.StatusCode < 400, nil
	}
	return resp.StatusCode == t.Expect, nil
}

// probePing runs one ICMP probe and reads the exit code only. -W 2 bounds one
// probe the way probeTimeout bounds a dial, so a wedged ICMP path reports down
// instead of hanging the sweep.
func probePing(ctx context.Context, t store.CheckTarget) (bool, error) {
	err := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "2", t.Address).Run()
	if err != nil {
		return false, nil
	}
	return true, nil
}

func checkStatus(up bool) string {
	if up {
		return store.CheckUp
	}
	return store.CheckDown
}
