package sched

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
)

// Trigger is the queue.Spec.Trigger every fire this package produces carries.
const Trigger = "schedule"

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
	runCtx  context.Context
	cancel  context.CancelFunc
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
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron != nil {
		return fmt.Errorf("sched: already started")
	}

	s.runCtx, s.cancel = context.WithCancel(ctx)
	s.cron = cron.New(cron.WithSeconds())
	if err := s.loadLocked(ctx); err != nil {
		s.cancel()
		s.cron, s.runCtx, s.cancel = nil, nil, nil
		return err
	}
	s.cron.Start()
	return nil
}

// Stop stops future fires and waits for any fire already in progress —
// including its jitter sleep — to finish enqueueing before returning.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	c := s.cron
	cancel := s.cancel
	s.cron, s.runCtx, s.cancel = nil, nil, nil
	s.entries = map[int64]cron.EntryID{}
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
func (s *Scheduler) Reload(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron == nil {
		return fmt.Errorf("sched: not started")
	}
	for _, id := range s.entries {
		s.cron.Remove(id)
	}
	s.entries = map[int64]cron.EntryID{}
	return s.loadLocked(ctx)
}

// loadLocked registers every enabled schedule. Callers hold s.mu.
func (s *Scheduler) loadLocked(ctx context.Context) error {
	rows, err := s.store.ListSchedules(ctx, true)
	if err != nil {
		return err
	}
	for _, sc := range rows {
		id, err := s.cron.AddFunc(sc.CronExpr, func() { s.fire(sc) })
		if err != nil {
			return fmt.Errorf("sched: register %q: %w", sc.Name, err)
		}
		s.entries[sc.ID] = id
	}
	return nil
}

// fire runs one schedule: it sleeps up to JitterS seconds, builds the specs
// the schedule's kind produces, enqueues every one of them, and stamps the
// schedule's last_job_id and next_at. It is a cron.Job's Run method, called
// in its own goroutine by the cron package.
func (s *Scheduler) fire(sc store.Schedule) {
	s.mu.Lock()
	ctx := s.runCtx
	s.mu.Unlock()
	if ctx == nil {
		return // Stop() raced this fire; nothing left to enqueue into.
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

	specs, err := s.buildSpecs(ctx, sc)
	if err != nil {
		s.log.Error("sched: build spec", "schedule", sc.Name, "err", err)
		return
	}

	var lastJobID *int64
	for _, spec := range specs {
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
