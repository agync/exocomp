package reactive

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/store"
)

// A drift incident is per-workspace: its GoAlert identity is Source=drift,
// Host=workspace, stable across every plan that observes the same drift and
// across the clean plan that clears it, so action=close finds the incident
// action=open raised. This mirrors the approval surface, whose opening page and
// its resolve share one Source/Host (approval.go).

// driftClearPrefix namespaces the checkpoint a clean plan writes when it closes
// a workspace's open drift incident. It is deliberately distinct from the open
// marker's SourceDrift prefix ("drift:") so the two never collide: an open key
// is "drift:<ws>:<job>", a clear key is "drift-clear:<ws>:<job>".
const driftClearPrefix = "drift-clear"

// driftClearDedupKey names the open drift a clean plan resolved — keyed by the
// OPENING plan's job, not the clean one, so "is this workspace's latest drift
// still open" is a single existence check. Writing it once is what makes the
// auto-close idempotent: the next sweep sees the latest open already cleared
// and pages nothing.
func driftClearDedupKey(workspace string, openJobID int64) string {
	return fmt.Sprintf("%s:%s:%d", driftClearPrefix, workspace, openJobID)
}

// latestOpenDrift reports the workspace's most recent drift-open plan and
// whether that incident is still unresolved. It reads the same event ledger the
// open markers live in: the newest "drift:<ws>:<job>" event names the latest
// drift, and the absence of its matching clear checkpoint means it is still
// open. A workspace that never drifted, or whose latest drift a clean plan
// already cleared, reports open=false, so a clean plan then pages nothing.
func (e *Engine) latestOpenDrift(ctx context.Context, workspace string) (openJobID int64, open bool, err error) {
	events, err := e.store.ListEvents(ctx, store.EventFilter{Source: SourceDrift, Limit: driftScan})
	if err != nil {
		return 0, false, err
	}

	// Events come back newest first, so the first open marker for this
	// workspace is its latest drift.
	prefix := fmt.Sprintf("%s:%s:", SourceDrift, workspace)
	for _, ev := range events {
		rest, ok := strings.CutPrefix(ev.DedupKey, prefix)
		if !ok {
			continue
		}
		jobID, convErr := strconv.ParseInt(rest, 10, 64)
		if convErr != nil {
			continue
		}
		cleared, err := e.store.CountEventsByDedupKey(ctx, driftClearDedupKey(workspace, jobID))
		if err != nil {
			return 0, false, err
		}
		return jobID, cleared == 0, nil
	}
	return 0, false, nil
}

// clearDrift closes a workspace's open drift incident because a plan came back
// clean. It pages the resolve first, then writes the checkpoint — a crash
// between the two re-closes once on the next sweep, which is the louder trade
// SweepDrift already makes on the open side. The checkpoint carries the OPENING
// job in its key so latestOpenDrift sees the incident resolved; it records the
// clean job that cleared it for the audit trail.
func (e *Engine) clearDrift(ctx context.Context, workspace string, openJobID int64, cleanJob store.Job) error {
	e.notify(ctx, driftClearedNotification(workspace, cleanJob))

	_, err := e.store.CreateEvent(ctx, store.Event{
		Source:     SourceDrift,
		ReceivedAt: e.now(),
		DedupKey:   driftClearDedupKey(workspace, openJobID),
		Outcome:    store.OutcomeMatched,
		Reason: fmt.Sprintf("workspace %s drift cleared: plan %d found no changes, "+
			"closing the incident opened by plan %d", workspace, cleanJob.ID, openJobID),
		JobID: &cleanJob.ID,
	})
	return err
}

// driftOpenNotification is the page a drifted workspace raises. Its Source/Host
// are the stable per-workspace identity a GoAlert transport opens an incident
// on; the matching close reuses them. A fire-and-forget transport delivers it
// as an ordinary warning.
func driftOpenNotification(workspace string, job store.Job) notify.Notification {
	return notify.Notification{
		Title:    "workspace drifted: " + workspace,
		Body:     fmt.Sprintf("Plan %d found changes on workspace %s.", job.ID, workspace),
		Priority: notify.PriorityHigh,
		Tags:     []string{"warning", fmt.Sprintf("id:%d", job.ID), "kind:drift"},
		Click:    driftClick(workspace),
		Source:   SourceDrift,
		Host:     workspace,
	}
}

// driftClick is the deep link every notification about one workspace's drift
// opens, matching the workspace-drift resource the MCP surface already
// exposes (exocomp://workspaces/{name}/drift) — an identifier, never the plan
// itself.
func driftClick(workspace string) string {
	return fmt.Sprintf("exocomp://workspaces/%s/drift", workspace)
}

// driftClearedNotification is the resolve: Resolved with the opening page's
// exact identity, so GoAlert closes the incident the drift opened and ntfy gets
// a terse "drift cleared". cleanJob is the plan that found nothing to change;
// it names the notification's id tag.
func driftClearedNotification(workspace string, cleanJob store.Job) notify.Notification {
	return notify.Notification{
		Title:    "drift cleared: " + workspace,
		Body:     fmt.Sprintf("A later plan found no changes on workspace %s.", workspace),
		Priority: notify.PriorityDefault,
		Tags:     []string{fmt.Sprintf("id:%d", cleanJob.ID), "kind:drift"},
		Click:    driftClick(workspace),
		Source:   SourceDrift,
		Host:     workspace,
		Resolved: true,
	}
}
