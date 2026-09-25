package reactive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"exocomp.dev/exocomp/internal/probe"
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

// probeConcurrency bounds how many of one check's targets are probed at once.
// The store has one connection and state application stays single-threaded; the
// bound just keeps a many-target check's wall time from summing its probes'
// timeouts.
const probeConcurrency = 4

// checkDedupKey names one target of one check. It is the condition the
// pipeline dedups on and a check-source rule's per-target cooldown keys off,
// so one target of one check is one slot.
func checkDedupKey(checkName, targetName string) string {
	return SourceCheck + ":" + checkName + ":" + targetName
}

// aggregateDedupKey names one check's aggregate condition. It is a single slot
// per check, distinct from every per-target key, and the stable identity the
// aggregate's resolve finds its firing by.
func aggregateDedupKey(checkName string) string {
	return SourceCheck + ":" + checkName + ":aggregate"
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

// probeResult is one target's probe outcome, kept in target order so state
// application and emission stay deterministic regardless of completion order.
type probeResult struct {
	target store.CheckTarget
	up     bool
	dur    time.Duration
	err    error
}

// probeAll probes every target of one check with a bounded worker pool.
// Results are written per index, so the caller applies them in the check's
// target order. A cancelled context marks the probes that never ran as erred,
// never as down.
func (e *Engine) probeAll(ctx context.Context, c store.Check) []probeResult {
	results := make([]probeResult, len(c.Targets))
	if len(c.Targets) == 0 {
		return results
	}
	workers := min(len(c.Targets), probeConcurrency)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				t := c.Targets[i]
				up, dur, err := e.probe(ctx, c, t)
				results[i] = probeResult{target: t, up: up, dur: dur, err: err}
			}
		}()
	}
send:
	for i := range c.Targets {
		select {
		case <-ctx.Done():
			for j := i; j < len(c.Targets); j++ {
				results[j] = probeResult{target: c.Targets[j], err: ctx.Err()}
			}
			break send
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

// anyObservedDown reports whether at least one target genuinely read down. A
// probe that errored is not a down observation, so it never consults the
// canary.
func anyObservedDown(results []probeResult) bool {
	for _, r := range results {
		if r.err == nil && !r.up {
			return true
		}
	}
	return false
}

// probeCanary dials the configured uplink canary and reports whether it
// answered. It is a bare TCP dial: the canary is a location on the path out
// (a gateway or a LAN service), not a service whose content matters, so exit
// truth is the whole verdict.
func probeCanary(ctx context.Context, canary string) bool {
	d := net.Dialer{Timeout: probeTimeout}
	conn, err := d.DialContext(ctx, "tcp", canary)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// sweepCheck probes every target of one check and updates check_state against
// each observation: a missing row is seeded without emitting, a steady state
// only advances the hysteresis counter, and a declared transition becomes one
// event. A broken target is logged and skipped, so one bad target cannot starve
// the check's other targets; only a cancelled context aborts the sweep.
func (e *Engine) sweepCheck(ctx context.Context, c store.Check, now time.Time) (int, error) {
	results := e.probeAll(ctx, c)

	// A vantage that lost its own uplink sees every target down. The canary is
	// the check that the vantage itself is reachable: when a target reads down
	// and the canary is also unreachable, this whole check's sweep is a missed
	// observation — no state advances, nothing emits — not a mass-down.
	if e.canary != "" && anyObservedDown(results) && !probeCanary(ctx, e.canary) {
		e.log.Warn("reactive: sweep checks: canary down; treating the sweep as a missed observation",
			"check", c.Name, "canary", e.canary)
		return 0, ctx.Err()
	}

	raised := 0
	snoozed := c.SnoozedUntil.After(now)
	for _, r := range results {
		if err := ctx.Err(); err != nil {
			return raised, err
		}
		t := r.target
		if r.err != nil {
			e.log.Error("reactive: sweep checks: probe", "check", c.Name, "target", t.Name, "err", r.err)
			continue
		}
		status := checkStatus(r.up)
		dur := r.dur

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

		// A snoozed check is still probed and its state still advances, so the
		// hysteresis is current when the snooze lapses; only the emission is
		// withheld.
		if !snoozed && st.Consecutive >= DefaultHysteresis && st.Emitted != st.Status {
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
	if c.Aggregate {
		n, err := e.sweepAggregate(ctx, c, results, snoozed, now)
		raised += n
		if err != nil {
			return raised, err
		}
	}
	return raised, ctx.Err() // nil when healthy
}

// sweepAggregate derives one check-level transition from the present target
// set, for an opt-in aggregate check only. The present set is the targets that
// answered up on this probe; the aggregate reads up once its size reaches the
// threshold (min_up, 0 meaning every target). Its transition state is the same
// shape as a target's, persisted under the reserved
// store.AggregateTargetName and advanced with the same hysteresis and
// cold-start seeding rules — a transition declares after three consecutive
// observations and a missing row is seeded without emitting. A snooze advances
// the state but withholds the emission, exactly as for a target.
func (e *Engine) sweepAggregate(ctx context.Context, c store.Check, results []probeResult, snoozed bool, now time.Time) (int, error) {
	present := make([]string, 0, len(results))
	for _, r := range results {
		if r.err == nil && r.up {
			present = append(present, r.target.Name)
		}
	}
	slices.Sort(present)

	threshold := c.MinUp
	if threshold == 0 {
		threshold = len(c.Targets)
	}
	status := store.CheckDown
	if len(present) >= threshold {
		status = store.CheckUp
	}

	st, err := e.store.GetCheckState(ctx, c.ID, store.AggregateTargetName)
	if errors.Is(err, store.ErrNotFound) {
		return 0, e.store.SetCheckState(ctx, store.CheckState{
			CheckID:            c.ID,
			TargetName:         store.AggregateTargetName,
			Status:             status,
			Consecutive:        1,
			Emitted:            status,
			FirstSeen:          now,
			LastChange:         now,
			RecentObservations: appendObservation("", status, 1, now),
		})
	}
	if err != nil {
		return 0, err
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

	raised := 0
	if !snoozed && st.Consecutive >= DefaultHysteresis && st.Emitted != st.Status {
		if err := e.emitAggregateTransition(ctx, c, st, present, now); err != nil {
			// The emitted flag is not advanced, so the next sweep re-observes
			// the same status and re-emits. Dedup-window-bounded, not a silent
			// loss, mirroring the per-target path.
			e.log.Error("reactive: sweep checks: emit aggregate transition", "check", c.Name, "err", err)
			return 0, err
		}
		st.Emitted = st.Status
		st.LastChange = now
		raised = 1
	}
	if err := e.store.SetCheckState(ctx, st); err != nil {
		return raised, err
	}
	return raised, nil
}

// emitAggregateTransition builds and ingests the one event an aggregate
// transition produces. The status maps onto firing/resolved the same way a
// target's does (up fires, down resolves), host is deliberately empty — the
// aggregate is not one host — and hosts carries the sorted present set the
// trusted-source line permits only a sweep to fill.
func (e *Engine) emitAggregateTransition(ctx context.Context, c store.Check, st store.CheckState, present []string, now time.Time) error {
	key := aggregateDedupKey(c.Name)

	eventStatus := StatusFiring
	if st.Status == store.CheckDown {
		eventStatus = StatusResolved
	}

	var ledger []byte
	if obs := transitionObservations(st.RecentObservations, st.Status); len(obs) > 0 {
		if b, merr := json.Marshal(obs); merr == nil {
			ledger = b
		}
	}

	total := len(c.Targets)
	ev := Event{
		Source:      SourceCheck,
		Kind:        "check",
		Name:        c.Name,
		Status:      eventStatus,
		Severity:    "warning",
		Host:        "",
		Fingerprint: key,
		Summary:     fmt.Sprintf("check %s aggregate is %s (%d of %d up)", c.Name, st.Status, len(present), total),
		Labels: map[string]string{
			"aggregate": "true",
			"up":        strconv.Itoa(len(present)),
			"total":     strconv.Itoa(total),
		},
		Annotations:       map[string]string{},
		ReceivedAt:        now,
		DedupKey:          key,
		CheckObservations: string(ledger),
		Hosts:             present,
	}

	raw, err := json.Marshal(map[string]any{
		"check": c.Name, "aggregate": true, "status": st.Status,
		"up": len(present), "total": total, "hosts": present,
	})
	if err != nil {
		return err
	}
	_, err = e.Ingest(ctx, ev, raw)
	return err
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

	// The probe's measured duration rides the event as a label so a rule can
	// act on "up but slow" (S3) without a second probe. It is the value already
	// persisted on the state row, not a new stored field.
	if labels == nil {
		labels = map[string]string{}
	}
	labels["duration_ms"] = strconv.FormatInt(st.LastDurationMS, 10)

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

// checkLabels builds the label set a check transition event carries. Only an
// address literal is a location verdict: tcp/ping (and, defensively, a tls
// address that is a bare IP) classify against the netgroups, "away" when none
// matches. An http URL target and the dns/tls/metric kinds carry no netgroup
// label — their liveness is not a location verdict — and the shared probe
// spec's Location is what decides, so a hostname or URL is never parsed as an
// address.
func (e *Engine) checkLabels(ctx context.Context, c store.Check, t store.CheckTarget) (map[string]string, error) {
	labels := map[string]string{"target": t.Name}
	spec, err := store.SpecForTarget(c.Kind, t)
	if err != nil {
		return nil, fmt.Errorf("sweep checks %s: target %q: %w", c.Name, t.Name, err)
	}
	addr, ok := spec.Location()
	if !ok {
		return labels, nil
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
// 3). A probe that cannot even start — a target whose spec does not validate —
// is an error, not a down, so a misconfigured check surfaces in the sweep
// warning rather than reporting silently all-down. The shared probe package
// decides it; a check refuses the loopback classes a gate allows.
func (e *Engine) probe(ctx context.Context, c store.Check, t store.CheckTarget) (up bool, dur time.Duration, err error) {
	spec, err := store.SpecForTarget(c.Kind, t)
	if err != nil {
		return false, 0, fmt.Errorf("sweep checks %s: %w", c.Name, err)
	}
	if err := spec.Validate(); err != nil {
		return false, 0, fmt.Errorf("sweep checks %s: %w", c.Name, err)
	}
	opts := probe.Options{Timeout: probeTimeout, AllowLoopback: false}
	if client := probeHTTPClient(); client != nil {
		opts.HTTPClient = client
	}
	res := probe.Run(ctx, spec, opts)
	return res.Up, res.Duration, nil
}

// probeHTTPClient is the test seam for the http probe's client. Nil uses the
// probe package's guarded default (a check refuses loopback, link-local,
// unspecified, and multicast); a test substitutes an unguarded client to reach
// a loopback test server without weakening the shipped guard.
var probeHTTPClient = func() *http.Client { return nil }

func checkStatus(up bool) string {
	if up {
		return store.CheckUp
	}
	return store.CheckDown
}
