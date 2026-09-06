package reactive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// A config-drift incident is per play subject, the way a tofu one is per
// workspace: its GoAlert identity is Source=drift, Host=<subject>, stable across
// every check that observes the same drift and across the clean check that
// clears it.
//
// ansibleDriftPrefix namespaces a check's open marker inside the drift ledger,
// and ansibleClearPrefix its close checkpoint. Both are deliberately distinct
// from the tofu prefixes ("drift:" / "drift-clear:"): a workspace and a play
// that happen to share a name must never resolve each other's incident.
const (
	ansibleDriftPrefix = "drift-play"
	ansibleClearPrefix = "drift-play-clear"
)

// AnsibleSubjectOf names the scope one `--check` play covers — the config-drift
// counterpart of a tofu job's workspace. A tofu job carries its workspace in the
// runbook; an ansible job carries no such registered noun, so the subject is
// built from the two identifiers the job row does have: the runbook (a play
// schedule's name — the operator-registered noun a recurring check fires under)
// and the playbook, which every playbook argv ends in.
//
// The playbook half is load-bearing, not decoration. Every manual `playbook run`
// shares the runbook "playbook", so a subject of runbook alone would let a clean
// check of one play close the incident a different play opened. Only the base
// name travels: the rest of the path is checkout-relative or absolute and moves
// between runs, and a subject that moves cannot close what it opened.
//
// ok is false for anything that is not a playbook run — an ad-hoc `ansible`
// module call, or an argv whose last element is a flag rather than a playbook.
func AnsibleSubjectOf(job store.Job) (string, bool) {
	if job.Runbook == "" {
		return "", false
	}
	argv := job.Args.Argv
	if len(argv) < 2 || argv[0] != executor.AnsibleBinary {
		return "", false
	}
	playbook := argv[len(argv)-1]
	if playbook == "" || strings.HasPrefix(playbook, "-") {
		return "", false
	}
	return job.Runbook + "/" + path.Base(playbook), true
}

// sweepAnsibleCheck settles one finished ansible job: a `--check` play whose
// recap reported changed tasks raises that subject's drift, and a later clean
// check clears the incident. It mirrors sweepTofuPlan step for step, so the two
// halves of a homelab page and close the same way.
func (e *Engine) sweepAnsibleCheck(ctx context.Context, job store.Job) (int, error) {
	subject, ok := AnsibleSubjectOf(job)
	if !ok {
		return 0, nil
	}
	outcome, ok, err := e.checkFoundChanges(ctx, job.ID)
	if err != nil || !ok {
		return 0, err
	}

	key := ansibleDriftDedupKey(subject, job.ID)
	seen, err := e.store.CountEventsByDedupKey(ctx, key)
	if err != nil {
		return 0, err
	}
	if seen > 0 {
		return 0, nil
	}

	if !outcome.Changes {
		// A run that could not reach every host saw nothing at all on the ones
		// it missed, so "no host reported a change" is not evidence the play is
		// clean. A degraded check leaves the incident open rather than closing
		// one on partial evidence. The degraded state is the truth; Partial is
		// the legacy marker still written beside it (D2).
		if job.Partial || job.State == store.StateDegraded {
			return 0, nil
		}
		openJob, open, err := e.latestOpenAnsibleDrift(ctx, subject)
		if err != nil || !open {
			return 0, err
		}
		return 0, e.clearAnsibleDrift(ctx, subject, openJob, job)
	}

	// Page before the checkpoint lands, the same louder trade sweepTofuPlan
	// makes: a crash between the two re-pages once rather than losing the page.
	e.notify(ctx, ansibleDriftOpenNotification(subject, job, outcome))

	ev := ansibleDriftEvent(subject, job, outcome, key, e.now())
	raw, err := json.Marshal(map[string]any{
		"subject": subject, "job": job.ID, "runbook": job.Runbook,
		"hosts": outcome.Drift,
	})
	if err != nil {
		return 0, err
	}
	if _, err := e.Ingest(ctx, ev, raw); err != nil {
		return 0, err
	}
	return 1, nil
}

// checkFoundChanges reads the check-mode outcome back out of a job's own events,
// which is where the ansible driver put it: the per-host recap is the only
// durable record of what a dry run would have changed. ok is false for an
// ordinary converge, which carries no check payload and is therefore no evidence
// about drift in either direction.
func (e *Engine) checkFoundChanges(ctx context.Context, jobID int64) (executor.AnsibleCheckOutcome, bool, error) {
	events, err := e.store.ListJobEvents(ctx, jobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return executor.AnsibleCheckOutcome{}, false, nil
		}
		return executor.AnsibleCheckOutcome{}, false, err
	}
	for _, ev := range events {
		if outcome, ok := executor.AnsibleCheckChanges(ev.RawJSON); ok {
			return outcome, true, nil
		}
	}
	return executor.AnsibleCheckOutcome{}, false, nil
}

// ansibleDriftDedupKey names the job that found the drift, so one check raises
// one event however often the sweep runs. Like driftDedupKey it deliberately
// does not collapse two checks of the same subject: each check is a fresh
// observation.
func ansibleDriftDedupKey(subject string, jobID int64) string {
	return fmt.Sprintf("%s:%s:%d", ansibleDriftPrefix, subject, jobID)
}

// ansibleClearDedupKey names the open drift a clean check resolved — keyed by
// the OPENING check's job, not the clean one, so "is this subject's latest drift
// still open" is a single existence check.
func ansibleClearDedupKey(subject string, openJobID int64) string {
	return fmt.Sprintf("%s:%s:%d", ansibleClearPrefix, subject, openJobID)
}

// latestOpenAnsibleDrift reports the subject's most recent drift-open check and
// whether that incident is still unresolved, the twin of latestOpenDrift over
// the ansible markers.
func (e *Engine) latestOpenAnsibleDrift(ctx context.Context, subject string) (openJobID int64, open bool, err error) {
	events, err := e.store.ListEvents(ctx, store.EventFilter{Source: SourceDrift, Limit: driftScan})
	if err != nil {
		return 0, false, err
	}

	// Events come back newest first, so the first open marker for this subject
	// is its latest drift.
	prefix := fmt.Sprintf("%s:%s:", ansibleDriftPrefix, subject)
	for _, ev := range events {
		rest, ok := strings.CutPrefix(ev.DedupKey, prefix)
		if !ok {
			continue
		}
		jobID, convErr := strconv.ParseInt(rest, 10, 64)
		if convErr != nil {
			// A longer subject whose key merely starts with this one's prefix.
			continue
		}
		cleared, err := e.store.CountEventsByDedupKey(ctx, ansibleClearDedupKey(subject, jobID))
		if err != nil {
			return 0, false, err
		}
		return jobID, cleared == 0, nil
	}
	return 0, false, nil
}

// clearAnsibleDrift closes a subject's open incident because a check came back
// clean. It pages the resolve first, then writes the checkpoint — the same trade
// clearDrift makes on the tofu side.
func (e *Engine) clearAnsibleDrift(ctx context.Context, subject string, openJobID int64, cleanJob store.Job) error {
	e.notify(ctx, ansibleDriftClearedNotification(subject, cleanJob))

	_, err := e.store.CreateEvent(ctx, store.Event{
		Source:     SourceDrift,
		ReceivedAt: e.now(),
		DedupKey:   ansibleClearDedupKey(subject, openJobID),
		Outcome:    store.OutcomeMatched,
		Reason: fmt.Sprintf("play %s drift cleared: check %d found nothing to change, "+
			"closing the incident opened by check %d", subject, cleanJob.ID, openJobID),
		JobID: &cleanJob.ID,
	})
	return err
}

// ansibleDriftEvent is what a check that found changes looks like once it is in
// the pipeline: an alert named AnsibleDrift beside TofuDrift, so a CEL rule
// gates a converge on config drift exactly as it gates an apply on tofu drift.
//
// The per-host summary rides in the labels as names and counts only. A task's
// diff can carry a secret; how many tasks would move on a host cannot.
func ansibleDriftEvent(subject string, job store.Job, o executor.AnsibleCheckOutcome,
	key string, now time.Time) Event {
	return Event{
		Source:      SourceDrift,
		Kind:        "drift",
		Name:        "AnsibleDrift",
		Status:      StatusFiring,
		Severity:    "warning",
		Host:        subject,
		Fingerprint: key,
		Summary: fmt.Sprintf("play %s has drifted: check %d would change %d task(s) on %d host(s)",
			subject, job.ID, changedTasks(o), len(o.Drift)),
		Labels: map[string]string{
			"subject": subject,
			"runbook": job.Runbook,
			"job":     fmt.Sprint(job.ID),
			"trigger": job.Trigger,
			"hosts":   strings.Join(driftedHosts(o), ","),
		},
		Annotations: map[string]string{},
		ReceivedAt:  now,
		DedupKey:    key,
	}
}

// ansibleDriftOpenNotification is the page a drifted play raises. Its
// Source/Host are the stable per-subject identity a GoAlert transport opens an
// incident on, at the priority tofu drift pages at; the matching close reuses
// them.
func ansibleDriftOpenNotification(subject string, job store.Job,
	o executor.AnsibleCheckOutcome) notify.Notification {
	return notify.Notification{
		Title: "play drifted: " + subject,
		Body: fmt.Sprintf("Check %d found %d task(s) that would change on %s.",
			job.ID, changedTasks(o), strings.Join(driftedHosts(o), ", ")),
		Priority: notify.PriorityHigh,
		Tags:     []string{"warning", fmt.Sprintf("id:%d", job.ID), "kind:drift"},
		Click:    fmt.Sprintf("exocomp://jobs/%d", job.ID),
		Source:   SourceDrift,
		Host:     subject,
	}
}

// ansibleDriftClearedNotification is the resolve: Resolved with the opening
// page's exact identity, so GoAlert closes the incident the drift opened.
// cleanJob is the check that found nothing to change; a play has no
// standalone resource of its own to link to, so the click names the check
// that cleared it, the same as the open notification names the one that
// raised it.
func ansibleDriftClearedNotification(subject string, cleanJob store.Job) notify.Notification {
	return notify.Notification{
		Title:    "drift cleared: " + subject,
		Body:     fmt.Sprintf("A later check found nothing to change on play %s.", subject),
		Priority: notify.PriorityDefault,
		Tags:     []string{fmt.Sprintf("id:%d", cleanJob.ID), "kind:drift"},
		Click:    fmt.Sprintf("exocomp://jobs/%d", cleanJob.ID),
		Source:   SourceDrift,
		Host:     subject,
		Resolved: true,
	}
}

func changedTasks(o executor.AnsibleCheckOutcome) int {
	total := 0
	for _, d := range o.Drift {
		total += d.Changed
	}
	return total
}

func driftedHosts(o executor.AnsibleCheckOutcome) []string {
	hosts := make([]string, len(o.Drift))
	for i, d := range o.Drift {
		hosts[i] = d.Host
	}
	return hosts
}
