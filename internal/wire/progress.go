package wire

import (
	"math"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// JobProgressView provides a concise progress snapshot of a job.
type JobProgressView struct {
	Play           string  `json:"play,omitempty"`
	Task           string  `json:"task,omitempty"`
	ElapsedSeconds float64 `json:"elapsed_seconds,omitempty"`
}

// ExtractJobProgress inspects a job's events and timing to return a concise
// progress view for a running job. Returns nil if the job is not running.
func ExtractJobProgress(j store.Job, events []store.JobEvent) *JobProgressView {
	if j.State != store.StateRunning || j.StartedAt == nil {
		return nil
	}

	var play, task string
	for _, e := range events {
		msg := strings.TrimSpace(e.Msg)
		if strings.HasPrefix(msg, "PLAY [") {
			if end := strings.Index(msg, "]"); end > 6 {
				play = msg[6:end]
			}
		} else if strings.HasPrefix(msg, "TASK [") {
			if end := strings.Index(msg, "]"); end > 6 {
				task = msg[6:end]
			}
		}
	}

	elapsed := time.Since(*j.StartedAt).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	elapsed = math.Round(elapsed*10) / 10

	return &JobProgressView{
		Play:           play,
		Task:           task,
		ElapsedSeconds: elapsed,
	}
}
