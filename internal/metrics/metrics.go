// Package metrics exposes the daemon's self-observability as a Prometheus
// text endpoint. The metrics that matter for a one-operator homelab are almost
// all staleness: not "is a request slow" but "when did this last succeed, and
// is that too long ago". A backup that stopped running, a drill that never
// passes, an approval nobody answered, a queue that stopped draining — each is
// a last-success timestamp drifting away from now, and each has an alert rule
// keyed on exactly that gap (alerts.yml, shipped beside this package).
package metrics

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"syscall"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// Registry gathers the daemon's staleness gauges from the store on demand.
// Nothing is cached: a scrape is a set of point-in-time reads, so a gauge is
// never staler than the moment it was asked for.
type Registry struct {
	store   *store.Store
	dataDir string
	now     func() time.Time
}

// New returns a registry reading from the store. dataDir is the path whose
// free space the disk gauge reports; an empty dataDir omits that gauge rather
// than reporting a misleading zero.
func New(st *store.Store, dataDir string) *Registry {
	return &Registry{store: st, dataDir: dataDir, now: func() time.Time { return time.Now().UTC() }}
}

// Handler serves the gauges in Prometheus text exposition format. A gather
// failure is a 500 with the reason: a scrape that half-succeeded would let an
// alert evaluate against a partial picture, which is worse than a gap the
// scraper already knows how to treat as stale.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := r.Render(req.Context())
		if err != nil {
			http.Error(w, "# gather failed: "+err.Error()+"\n", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
}

// gauge is one emitted sample: a metric family name, its labels, and value.
type gauge struct {
	name   string
	help   string
	labels map[string]string
	value  float64
}

// Render gathers every gauge and formats the exposition text.
func (r *Registry) Render(ctx context.Context) (string, error) {
	gauges, err := r.gather(ctx)
	if err != nil {
		return "", err
	}
	return format(gauges), nil
}

func (r *Registry) gather(ctx context.Context) ([]gauge, error) {
	now := r.now()
	out := []gauge{{name: "exocomp_up", help: "1 when the daemon is serving this endpoint.", value: 1}}

	backups, err := r.backupGauges(ctx, now)
	if err != nil {
		return nil, err
	}
	out = append(out, backups...)

	drift, err := r.workspaceGauges(ctx, now)
	if err != nil {
		return nil, err
	}
	out = append(out, drift...)

	queue, err := r.queueGauge(ctx, now)
	if err != nil {
		return nil, err
	}
	out = append(out, queue...)

	appr, err := r.approvalGauges(ctx, now)
	if err != nil {
		return nil, err
	}
	out = append(out, appr...)

	held, err := r.holdGauges(ctx, now)
	if err != nil {
		return nil, err
	}
	out = append(out, held...)

	ls, err := r.litestreamGauges(ctx, now)
	if err != nil {
		return nil, err
	}
	out = append(out, ls...)

	if g, ok := r.diskGauge(); ok {
		out = append(out, g)
	}
	return out, nil
}

const (
	helpBackupTS      = "Unix time of the last successful run of a backup leg."
	helpBackupStale   = "Seconds since a backup leg last succeeded (+Inf if it never has)."
	helpDriftTS       = "Unix time of the last successful tofu run for a workspace."
	helpDriftStale    = "Seconds since a workspace was last checked for drift."
	helpQueueOldest   = "Age in seconds of the oldest job still queued (0 when the queue is empty)."
	helpApprovalAge   = "Age in seconds of the oldest approval still waiting on a human."
	helpApprovalCount = "Number of approvals still waiting on a human."
	helpPaused        = "1 when a global silence is holding reactive jobs (the PAUSED banner)."
	helpMuted         = "1 when a notify mute is holding notifications (the MUTED banner)."
	helpDisk          = "Fraction of the data-dir filesystem in use, 0..1."
)

func (r *Registry) backupGauges(ctx context.Context, now time.Time) ([]gauge, error) {
	targets, err := r.store.ListBackupTargets(ctx, false)
	if err != nil {
		return nil, err
	}
	var out []gauge
	for _, t := range targets {
		labels := map[string]string{"target": t.Name}
		last, ok, err := r.lastSuccessfulBackup(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			out = append(out, gauge{name: "exocomp_backup_staleness_seconds", help: helpBackupStale, labels: labels, value: inf()})
			continue
		}
		out = append(out,
			gauge{name: "exocomp_backup_last_success_timestamp_seconds", help: helpBackupTS, labels: labels, value: unix(last)},
			gauge{name: "exocomp_backup_staleness_seconds", help: helpBackupStale, labels: labels, value: age(now, last)},
		)
	}
	return out, nil
}

// lastSuccessfulBackup returns the finish time of the newest succeeded run of a
// target. ListBackupRuns is newest-first, so the first succeeded row wins.
func (r *Registry) lastSuccessfulBackup(ctx context.Context, targetID int64) (time.Time, bool, error) {
	runs, err := r.store.ListBackupRuns(ctx, targetID, 0)
	if err != nil {
		return time.Time{}, false, err
	}
	for _, run := range runs {
		if run.State == store.BackupSucceeded && run.FinishedAt != nil {
			return *run.FinishedAt, true, nil
		}
	}
	return time.Time{}, false, nil
}

func (r *Registry) workspaceGauges(ctx context.Context, now time.Time) ([]gauge, error) {
	workspaces, err := r.store.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	var out []gauge
	for _, ws := range workspaces {
		labels := map[string]string{"workspace": ws.Name}
		last, ok, err := r.lastWorkspaceCheck(ctx, ws.Name)
		if err != nil {
			return nil, err
		}
		if !ok {
			out = append(out, gauge{name: "exocomp_workspace_drift_check_staleness_seconds", help: helpDriftStale, labels: labels, value: inf()})
			continue
		}
		out = append(out,
			gauge{name: "exocomp_workspace_drift_check_timestamp_seconds", help: helpDriftTS, labels: labels, value: unix(last)},
			gauge{name: "exocomp_workspace_drift_check_staleness_seconds", help: helpDriftStale, labels: labels, value: age(now, last)},
		)
	}
	return out, nil
}

// lastWorkspaceCheck returns the finish time of the newest succeeded tofu job
// (plan or apply) for a workspace: the last moment its real state was compared
// against its config.
func (r *Registry) lastWorkspaceCheck(ctx context.Context, name string) (time.Time, bool, error) {
	jobs, err := r.store.ListJobs(ctx, store.JobFilter{
		Runbook: "workspace:" + name,
		States:  []store.JobState{store.StateSucceeded},
	})
	if err != nil {
		return time.Time{}, false, err
	}
	var latest time.Time
	found := false
	for _, j := range jobs {
		if j.FinishedAt != nil && j.FinishedAt.After(latest) {
			latest = *j.FinishedAt
			found = true
		}
	}
	return latest, found, nil
}

func (r *Registry) queueGauge(ctx context.Context, now time.Time) ([]gauge, error) {
	jobs, err := r.store.ListJobs(ctx, store.JobFilter{States: []store.JobState{store.StateQueued}})
	if err != nil {
		return nil, err
	}
	oldest := 0.0
	for _, j := range jobs {
		if a := age(now, j.QueuedAt); a > oldest {
			oldest = a
		}
	}
	return []gauge{{name: "exocomp_queue_oldest_job_age_seconds", help: helpQueueOldest, value: oldest}}, nil
}

func (r *Registry) approvalGauges(ctx context.Context, now time.Time) ([]gauge, error) {
	pending, err := r.store.ListApprovals(ctx, true)
	if err != nil {
		return nil, err
	}
	oldest := 0.0
	for _, a := range pending {
		if v := age(now, a.RequestedAt); v > oldest {
			oldest = v
		}
	}
	return []gauge{
		{name: "exocomp_pending_approval_oldest_age_seconds", help: helpApprovalAge, value: oldest},
		{name: "exocomp_pending_approvals", help: helpApprovalCount, value: float64(len(pending))},
	}, nil
}

// holdGauges reports the two banner states as gauges: a global silence is
// PAUSED, an active notify mute is MUTED. They are separate rows so an alert
// can fire on a forgotten pause without firing on an intended mute.
func (r *Registry) holdGauges(ctx context.Context, _ time.Time) ([]gauge, error) {
	silences, err := r.store.ListSilences(ctx, true)
	if err != nil {
		return nil, err
	}
	paused := 0.0
	for _, s := range silences {
		if s.ScopeKind == store.ScopeGlobal {
			paused = 1
			break
		}
	}
	mutes, err := r.store.ListNotifyMutes(ctx, true)
	if err != nil {
		return nil, err
	}
	muted := 0.0
	if len(mutes) > 0 {
		muted = 1
	}
	return []gauge{
		{name: "exocomp_reactive_paused", help: helpPaused, value: paused},
		{name: "exocomp_notify_muted", help: helpMuted, value: muted},
	}, nil
}

func (r *Registry) diskGauge() (gauge, bool) {
	if r.dataDir == "" {
		return gauge{}, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(r.dataDir, &st); err != nil {
		return gauge{}, false
	}
	total := st.Blocks
	if total == 0 {
		return gauge{}, false
	}
	used := float64(total-st.Bavail) / float64(total)
	return gauge{name: "exocomp_disk_used_ratio", help: helpDisk, labels: map[string]string{"path": r.dataDir}, value: used}, true
}

func unix(t time.Time) float64 { return float64(t.UTC().UnixMilli()) / 1000 }

func age(now, t time.Time) float64 {
	d := now.Sub(t).Seconds()
	if d < 0 {
		return 0
	}
	return d
}

// inf is +Inf, the staleness of a leg that has never succeeded: an alert on
// "stale for more than N" then fires without a special never-ran case.
func inf() float64 { return math.Inf(1) }

// format writes the gauges as exposition text: each family's HELP and TYPE
// once, then its samples, families in stable name order so a diff of two
// scrapes is about values, not ordering.
func format(gauges []gauge) string {
	families := map[string][]gauge{}
	help := map[string]string{}
	var order []string
	for _, g := range gauges {
		if _, seen := families[g.name]; !seen {
			order = append(order, g.name)
		}
		families[g.name] = append(families[g.name], g)
		if g.help != "" {
			help[g.name] = g.help
		}
	}
	sort.Strings(order)

	var b strings.Builder
	for _, name := range order {
		if h := help[name]; h != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", name, h)
		}
		fmt.Fprintf(&b, "# TYPE %s gauge\n", name)
		samples := families[name]
		sort.SliceStable(samples, func(i, j int) bool {
			return labelString(samples[i].labels) < labelString(samples[j].labels)
		})
		for _, s := range samples {
			fmt.Fprintf(&b, "%s%s %s\n", name, labelString(s.labels), formatValue(s.value))
		}
	}
	return b.String()
}

func labelString(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, escapeLabel(labels[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// formatValue renders a float the way the exposition format wants it: +Inf for
// the never-succeeded sentinel, a plain integer when the value is whole, and a
// trimmed decimal otherwise.
func formatValue(v float64) string {
	if v >= 1e308 {
		return "+Inf"
	}
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}
