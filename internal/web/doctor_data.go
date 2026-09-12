package web

import (
	"context"
	"encoding/json"

	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// parseDoctorFindings reads a doctor job's structured finding events into the
// rows the job page renders — the same wire.HealthEntry shape the daemon's
// doctor driver marshals as each event's RawJSON, never a second probe.
func parseDoctorFindings(events []store.JobEvent) []doctorFindingRow {
	var out []doctorFindingRow
	for _, e := range events {
		if e.RawJSON == "" {
			continue
		}
		var f struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail,omitempty"`
		}
		if err := json.Unmarshal([]byte(e.RawJSON), &f); err != nil || f.Name == "" {
			continue
		}
		out = append(out, doctorFindingRow{
			Name: f.Name, Status: f.Status, Detail: f.Detail, Class: doctorFindingClass(f.Status),
		})
	}
	return out
}

// countDoctorFindings is the identity strip's finding count: how many of a
// doctor job's findings need attention. Unknown is a state, not a finding.
func countDoctorFindings(events []store.JobEvent) int {
	n := 0
	for _, f := range parseDoctorFindings(events) {
		if f.Status == wire.HealthWarn || f.Status == wire.HealthFail {
			n++
		}
	}
	return n
}

// doctorMeta reads the newest kind=doctor job and its findings: the age the
// identity strip shows and the count of findings that need attention. The
// count is parsed from the job's own structured finding events — the same
// rows the job page renders — so the strip and the page can never disagree.
func (s *Server) doctorMeta(ctx context.Context) doctorMeta {
	jobs, err := s.store.ListJobs(ctx, store.JobFilter{Runbook: health.JobRunbook})
	if err != nil || len(jobs) == 0 {
		return doctorMeta{}
	}
	job := jobs[len(jobs)-1]
	when := job.FinishedAt
	if when == nil {
		when = job.StartedAt
	}
	if when == nil {
		when = &job.QueuedAt
	}
	m := doctorMeta{HasRun: true, JobID: job.ID, Age: humanAge(s.now().Sub(*when))}
	if events, err := s.store.ListJobEvents(ctx, job.ID); err == nil {
		m.Count = countDoctorFindings(events)
	}
	m.Stale = m.Count > 0
	return m
}
