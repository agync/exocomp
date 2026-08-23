package sched

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/store"
)

// Trigger is the queue.Spec.Trigger every fire this package produces carries.
const Trigger = "schedule"

// parser is the one cron dialect this project speaks: six fields with seconds
// first, plus the "@every"/"@daily" descriptors. It is exported through
// ValidateCron so `schedule add` refuses at write time exactly what the
// scheduler would refuse at read time.
var parser = cron.NewParser(cron.Second | cron.Minute | cron.Hour |
	cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ValidateCron reports whether expr is an expression the scheduler can
// register.
func ValidateCron(expr string) error {
	if _, err := parser.Parse(expr); err != nil {
		return fmt.Errorf("sched: %q is not a cron expression: %w", expr, err)
	}
	return nil
}

// Enqueuer is the subset of *queue.Queue the scheduler needs. It exists so
// tests can substitute a fake that records what a fire enqueues without
// running a full queue; *queue.Queue satisfies it as-is.
type Enqueuer interface {
	Enqueue(ctx context.Context, spec queue.Spec) (store.Job, error)
}

// Scheduler loads schedules from a store and fires them into a queue on
// their cron expression. Every fire goes through Enqueue, the same path a
// keypress or a webhook rule uses — there is no side door here.
type Scheduler struct {
	store *store.Store
	queue Enqueuer
	log   *slog.Logger

	mu      sync.Mutex
	cron    *cron.Cron
	entries map[int64]cron.EntryID
	// fingerprint is what the running registrations were built from. A reload
	// that reads the same rows back keeps them, because rebuilding restarts
	// every entry's clock: an "@every 1h" registration rebuilt on a shorter
	// reload interval would have its next fire pushed out forever and never
	// run at all.
	fingerprint string
	runCtx      context.Context
	cancel      context.CancelFunc
	// paused suppresses firing without tearing the clock down: a cordoned daemon
	// stops enqueueing scheduled work ahead of a planned reboot, and because a
	// suppressed fire never stamps last_run, Resume's catch-up settles whatever
	// occurrences were missed while paused — the same coalescing a downtime gets.
	paused bool
}

// New returns a Scheduler over st and q. log defaults to slog.Default().
func New(st *store.Store, q Enqueuer, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{store: st, queue: q, log: log, entries: map[int64]cron.EntryID{}}
}

// Start loads every enabled schedule and registers it with a cron instance
// that parses six fields (seconds first) and the "@every" form. Fires
// spawned after Start use a context derived from ctx for their jitter sleep
// and their Enqueue call, so cancelling ctx cancels any fire in flight too.
//
// A row the parser rejects is logged and skipped: one unregisterable schedule
// must not cost the daemon every other one, nor the queue and the state
// backend that start with it.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.cron != nil {
		s.mu.Unlock()
		return fmt.Errorf("sched: already started")
	}

	c, entries, fp, err := s.build(ctx)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.runCtx, s.cancel = context.WithCancel(ctx)
	s.cron, s.entries, s.fingerprint = c, entries, fp
	s.cron.Start()
	runCtx := s.runCtx
	s.mu.Unlock()

	// Catch up after the clock is running so a coalesced fire can read its own
	// next scheduled time back off the live entry.
	s.catchUp(runCtx)
	return nil
}

// Stop stops future fires and waits for any fire already in progress —
// including its jitter sleep — to finish enqueueing before returning.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	c := s.cron
	cancel := s.cancel
	s.cron, s.runCtx, s.cancel = nil, nil, nil
	s.entries, s.fingerprint = map[int64]cron.EntryID{}, ""
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if c != nil {
		<-c.Stop().Done()
	}
}

// Reload rebuilds every registration from the database: schedules added,
// re-enabled, disabled, or edited since Start (or the last Reload) take
// effect. It requires the scheduler to already be started.
//
// The new set is built whole before anything is swapped, so a reload that
// cannot read the database leaves the running registrations alone rather than
// unregistering every schedule on the way to failing.
func (s *Scheduler) Reload(ctx context.Context) error {
	s.mu.Lock()
	if s.cron == nil {
		s.mu.Unlock()
		return fmt.Errorf("sched: not started")
	}

	c, entries, fp, err := s.build(ctx)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if fp == s.fingerprint {
		// Nothing changed. Installing an identical set would still restart
		// every entry's clock, which for a relative expression means the next
		// fire is pushed out by one whole period on every reload.
		c.Stop()
		runCtx := s.runCtx
		s.mu.Unlock()
		// A schedule re-enabled while the daemon was down reaches a running
		// daemon as a reload, not a start, so catch-up belongs here too. It is
		// a no-op for the common case: last_run advancement leaves nothing to
		// coalesce for a schedule that has been firing normally.
		s.catchUp(runCtx)
		return nil
	}
	old := s.cron
	s.cron, s.entries, s.fingerprint = c, entries, fp
	c.Start()
	// Stop only fires no more; a fire already running keeps its goroutine and
	// finishes against the entries this reload just installed.
	old.Stop()
	runCtx := s.runCtx
	s.mu.Unlock()

	s.catchUp(runCtx)
	return nil
}

// build assembles a cron instance holding every enabled schedule the parser
// accepts, plus the fingerprint of what it was built from. An error means the
// database could not be read at all — a row that will not register is logged
// and left out.
func (s *Scheduler) build(ctx context.Context) (*cron.Cron, map[int64]cron.EntryID, string, error) {
	rows, err := s.store.ListSchedules(ctx, true)
	if err != nil {
		return nil, nil, "", err
	}

	c := cron.New(cron.WithParser(parser))
	entries := make(map[int64]cron.EntryID, len(rows))
	for _, sc := range rows {
		id, err := c.AddFunc(sc.CronExpr, func() { s.fire(sc) })
		if err != nil {
			s.log.Error("sched: schedule not registered",
				"schedule", sc.Name, "cron", sc.CronExpr, "err", err)
			continue
		}
		entries[sc.ID] = id
	}
	return c, entries, fingerprint(rows), nil
}

// fingerprint reduces the rows a build registered to a value two builds can be
// compared by. It covers everything a fire acts on — the expression, the kind,
// the args, the concurrency key, the jitter — and nothing that only records
// history, so a stamped last_job_id does not read as a change.
func fingerprint(rows []store.Schedule) string {
	var b strings.Builder
	for _, sc := range rows {
		args, err := json.Marshal(sc.Args)
		if err != nil {
			// Unreachable: ScheduleArgs is a string slice and two strings.
			args = []byte(err.Error())
		}
		fmt.Fprintf(&b, "%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x1e",
			sc.ID, sc.Name, sc.CronExpr, sc.Kind, args, sc.ConcurrencyKey, sc.JitterS)
	}
	return b.String()
}

// Pause suppresses firing while leaving the clock running: cron entries keep
// advancing, but a fire that lands while paused enqueues nothing and, crucially,
// does not stamp last_run. It is the scheduler half of a cordon.
func (s *Scheduler) Pause() {
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
}

// Resume re-enables firing and then runs catch-up, so any occurrence that passed
// while paused fires once (coalesced), the same way a schedule caught up after
// downtime. It is safe to call when not paused (catch-up is then a no-op) and
// before Start (nothing to catch up).
func (s *Scheduler) Resume() {
	s.mu.Lock()
	s.paused = false
	ctx := s.runCtx
	s.mu.Unlock()
	s.catchUp(ctx)
}

// Paused reports whether firing is currently suppressed.
func (s *Scheduler) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// fire runs one schedule on its cron tick: it sleeps up to JitterS seconds,
// then dispatches. It is a cron.Job's Run method, called in its own goroutine
// by the cron package.
func (s *Scheduler) fire(sc store.Schedule) {
	s.mu.Lock()
	ctx := s.runCtx
	paused := s.paused
	s.mu.Unlock()
	if ctx == nil {
		return // Stop() raced this fire; nothing left to enqueue into.
	}
	if paused {
		return // Cordoned: suppress the fire; last_run is left for Resume to catch up.
	}

	if sc.JitterS > 0 {
		jitterMax := int64(sc.JitterS) * int64(time.Second)
		timer := time.NewTimer(time.Duration(rand.Int64N(jitterMax + 1)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}

	s.dispatch(ctx, sc, "")
}

// catchUp fires, once, every enabled schedule that a cron occurrence passed
// between its last_run and now — the fires the daemon owed while it was down.
// It coalesces: a schedule down for a week gets one fire, not one per missed
// occurrence. A schedule with no last_run (brand-new) is left to fire forward
// only, and a schedule whose last_run is recent enough that no occurrence was
// missed does nothing. It runs after Start/Reload has released the lock and
// started the clock, so a caught-up fire can read its live next-fire time.
func (s *Scheduler) catchUp(ctx context.Context) {
	if ctx == nil {
		return
	}
	rows, err := s.store.ListSchedules(ctx, true)
	if err != nil {
		s.log.Error("sched: catch-up list", "err", err)
		return
	}
	now := time.Now()
	for _, sc := range rows {
		if sc.LastRun == nil {
			continue // Never fired: only ever schedule forward from now.
		}
		schedule, err := parser.Parse(sc.CronExpr)
		if err != nil {
			continue // build already logged the unregisterable row.
		}
		// The first occurrence strictly after last_run is the one owed. If it is
		// still in the future nothing was missed; if it has passed, one coalesced
		// fire settles however many occurrences went by.
		if next := schedule.Next(*sc.LastRun); next.IsZero() || next.After(now) {
			continue
		}
		s.dispatch(ctx, sc, catchUpKey(sc))
	}
}

// catchUpKey is the idempotency key a coalesced catch-up fire enqueues under.
// Anchoring it on last_run (not on now) makes the catch-up safe to retry: if
// the daemon enqueues the fire but dies before advancing last_run, the next
// boot recomputes the same key and the queue returns the existing job rather
// than a second one. Two overlapping reloads racing the same stale last_run
// collapse the same way.
func catchUpKey(sc store.Schedule) string {
	return fmt.Sprintf("sched-catchup:%d:%d", sc.ID, sc.LastRun.UnixMilli())
}

// dispatch builds a schedule's specs, enqueues each, and stamps last_run,
// last_job_id, and next_at. idem is empty for a normal cron fire and set for a
// catch-up, where it de-duplicates a fire the queue may already hold — a fan-out
// kind's per-target concurrency key keeps each target's fire distinct.
func (s *Scheduler) dispatch(ctx context.Context, sc store.Schedule, idem string) {
	specs, err := s.buildSpecs(ctx, sc)
	if err != nil {
		s.log.Error("sched: build spec", "schedule", sc.Name, "err", err)
		return
	}

	var lastJobID *int64
	for _, spec := range specs {
		if idem != "" {
			spec.IdempotencyKey = idem
			if spec.ConcurrencyKey != "" {
				spec.IdempotencyKey += ":" + spec.ConcurrencyKey
			}
		}
		job, err := s.queue.Enqueue(ctx, spec)
		if err != nil {
			s.log.Error("sched: enqueue", "schedule", sc.Name, "err", err)
			continue
		}
		id := job.ID
		lastJobID = &id
	}

	next := s.nextFireTime(sc.ID)
	if err := s.store.RecordScheduleFire(ctx, sc.ID, lastJobID, next); err != nil {
		s.log.Error("sched: stamp fire", "schedule", sc.Name, "err", err)
	}
}

// nextFireTime reads a schedule's next scheduled time from the live cron
// entry. It runs after cron has already advanced the entry past this fire
// (cron does so before running the job it just started), so the value is the
// fire after this one, not this one repeated.
func (s *Scheduler) nextFireTime(scheduleID int64) *time.Time {
	s.mu.Lock()
	c := s.cron
	id, ok := s.entries[scheduleID]
	s.mu.Unlock()
	if c == nil || !ok {
		return nil
	}
	next := c.Entry(id).Next
	if next.IsZero() {
		return nil
	}
	return &next
}

// buildSpecs turns one schedule into the queue.Spec(s) its kind produces. A
// backup schedule with an empty target fans out to every enabled target,
// each getting its own job and its own "backup:<target>" concurrency key.
func (s *Scheduler) buildSpecs(ctx context.Context, sc store.Schedule) ([]queue.Spec, error) {
	switch sc.Kind {
	case store.ScheduleShell:
		if len(sc.Args.Argv) == 0 {
			return nil, fmt.Errorf("schedule %q: kind shell needs argv", sc.Name)
		}
		return []queue.Spec{{
			Runbook:        sc.Name,
			Kind:           KindShell,
			Trigger:        Trigger,
			Argv:           sc.Args.Argv,
			ConcurrencyKey: sc.ConcurrencyKey,
		}}, nil

	case store.ScheduleTofuPlan:
		name := sc.Args.TofuWorkspace()
		if name == "" {
			return nil, fmt.Errorf("schedule %q: kind %s needs a workspace",
				sc.Name, store.ScheduleTofuPlan)
		}
		ws, err := s.store.WorkspaceByName(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("schedule %q: workspace %q: %w", sc.Name, name, err)
		}
		// Drift detection: the plan runs with -detailed-exitcode, and the
		// reactive engine turns "changes present" into an event in the same
		// pipeline a webhook enters, so drift can gate an apply.
		return []queue.Spec{reactive.PlanSpec(ws, Trigger)}, nil

	case KindPlay:
		spec, err := s.playSpec(ctx, sc)
		if err != nil {
			return nil, err
		}
		return []queue.Spec{spec}, nil

	case store.ScheduleBackup:
		names, err := s.backupTargetNames(ctx, sc.Args.Target)
		if err != nil {
			return nil, err
		}
		specs := make([]queue.Spec, 0, len(names))
		for _, name := range names {
			specs = append(specs, queue.Spec{
				Runbook:        sc.Name,
				Kind:           KindBackup,
				Trigger:        Trigger,
				Argv:           backup.Argv(name),
				ConcurrencyKey: backup.ConcurrencyKey(name),
			})
		}
		return specs, nil

	case store.ScheduleDrill:
		names, err := s.backupTargetNames(ctx, sc.Args.Target)
		if err != nil {
			return nil, err
		}
		specs := make([]queue.Spec, 0, len(names))
		for _, name := range names {
			specs = append(specs, queue.Spec{
				Runbook:        sc.Name,
				Kind:           backup.DrillJobKind,
				Trigger:        Trigger,
				Argv:           backup.DrillArgv(name),
				ConcurrencyKey: backup.DrillConcurrencyKey(name),
			})
		}
		return specs, nil

	default:
		return nil, fmt.Errorf("schedule %q: unknown kind %q", sc.Name, sc.Kind)
	}
}

// backupTargetNames resolves one target's name, or every enabled target's
// name when target is empty.
func (s *Scheduler) backupTargetNames(ctx context.Context, target string) ([]string, error) {
	if target != "" {
		return []string{target}, nil
	}
	targets, err := s.store.ListBackupTargets(ctx, false)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	return names, nil
}
