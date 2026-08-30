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
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// DefaultHysteresis is how many consecutive observations a status must hold
// before it becomes a declared transition and an event is emitted, in both
// directions. A target on marginal wifi cannot otherwise flap up/down/up —
// each flap would withdraw and re-raise a pending approval, churning the
// events table.
const DefaultHysteresis = 3

// probeTimeout bounds one probe: a dial, an http GET, or a ping subprocess, so
// a wedged target reports down instead of hanging the sweep.
const probeTimeout = 5 * time.Second

// checkDedupKey names one target of one check. It is the condition the
// pipeline dedups on and a check-source rule's per-target cooldown keys off,
// so one target of one check is one slot.
func checkDedupKey(checkName, targetName string) string {
	return SourceCheck + ":" + checkName + ":" + targetName
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
		return 0, err
	}
	now := e.now()

	raised := 0
	for _, c := range checks {
		if next, ok := e.checkDue(c.ID); ok && now.Before(next) {
			continue
		}
		n, err := e.sweepCheck(ctx, c, now)
		if err != nil {
			return raised, err
		}
		raised += n
		// The next probe lands one interval after this one on the engine
		// clock; the sweep tick floors the effective interval at its own
		// period (a 5s check on a 30s tick probes every 30s).
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
// event.
func (e *Engine) sweepCheck(ctx context.Context, c store.Check, now time.Time) (int, error) {
	raised := 0
	for _, t := range c.Targets {
		up, err := e.probe(ctx, c, t)
		if err != nil {
			return raised, err
		}
		status := checkStatus(up)

		st, err := e.store.GetCheckState(ctx, c.ID, t.Name)
		if errors.Is(err, store.ErrNotFound) {
			// Cold-start seed: observe and record without emitting. emitted is
			// seeded to the observed result, never '' — with a '' seed the
			// `emitted != status` transition condition would be true on every
			// steady observation, so a daemon restart would spurious-fire
			// every up target (design §5.2).
			if err := e.store.SetCheckState(ctx, store.CheckState{
				CheckID:     c.ID,
				TargetName:  t.Name,
				Status:      status,
				Consecutive: 1,
				Emitted:     status,
				FirstSeen:   now,
				LastChange:  now,
			}); err != nil {
				return raised, err
			}
			continue
		}
		if err != nil {
			return raised, err
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

		if st.Consecutive >= DefaultHysteresis && st.Emitted != st.Status {
			if err := e.emitCheckTransition(ctx, c, t, st.Status, now); err != nil {
				return raised, err
			}
			st.Emitted = st.Status
			st.LastChange = now
			raised++
		}

		if err := e.store.SetCheckState(ctx, st); err != nil {
			return raised, err
		}
	}
	return raised, nil
}

// emitCheckTransition builds and ingests the one event a declared transition
// produces. The status here is the check_state vocabulary (up/down); the event
// maps it onto firing/resolved so a resolve finds and withdraws what its
// firing raised.
func (e *Engine) emitCheckTransition(ctx context.Context, c store.Check, t store.CheckTarget, status string, now time.Time) error {
	key := checkDedupKey(c.Name, t.Name)

	eventStatus := StatusFiring
	if status == store.CheckDown {
		eventStatus = StatusResolved
	}

	labels := map[string]string{"target": t.Name}
	if t.Address != "" {
		addr, err := netip.ParseAddr(t.Address)
		if err != nil {
			return fmt.Errorf("sweep checks %s: target %q address %q does not parse: %w",
				c.Name, t.Name, t.Address, err)
		}
		group, err := e.store.NetgroupForAddr(ctx, addr)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			group = "away"
		}
		labels["netgroup"] = group
	}

	ev := Event{
		Source:      SourceCheck,
		Kind:        "check",
		Name:        c.Name,
		Status:      eventStatus,
		Severity:    "warning",
		Host:        t.Name,
		Fingerprint: key,
		Summary:     fmt.Sprintf("check %s target %s is %s", c.Name, t.Name, status),
		Labels:      labels,
		Annotations: map[string]string{},
		ReceivedAt:  now,
		DedupKey:    key,
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

// probe asks one target once and returns whether it answered. Results are
// dial/exit truth only, never parsed output (AGENTS.md 3). A probe that cannot
// even start — an unparseable address or URL — is an error, not a down, so a
// misconfigured check surfaces in the sweep warning rather than reporting
// silently all-down.
func (e *Engine) probe(ctx context.Context, c store.Check, t store.CheckTarget) (bool, error) {
	switch c.Kind {
	case store.CheckTCP:
		return probeTCP(ctx, t)
	case store.CheckPing:
		return probePing(ctx, t)
	case store.CheckHTTP:
		return probeHTTP(ctx, t)
	default:
		return false, fmt.Errorf("sweep checks %s: unknown kind %q", c.Name, c.Kind)
	}
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

// probeHTTP GETs the URL and reads the status line only. The body is closed
// without being read: a health endpoint's response is not evidence this code
// should persist (AGENTS.md 4). Expect 0 means any 2xx/3xx answers up;
// otherwise the status must match exactly.
func probeHTTP(ctx context.Context, t store.CheckTarget) (bool, error) {
	client := &http.Client{Timeout: probeTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
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
