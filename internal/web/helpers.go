package web

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
)

func itoa(n int64) string   { return strconv.FormatInt(n, 10) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func workspaceSuffix(ws string) string {
	if ws == "" {
		return ""
	}
	return " · " + ws
}

func swallowedText(b bannerVM) string {
	// A notification mute holds the push, it does not swallow alerts, so a
	// mute-only banner reports the held-push fact instead of a zero count.
	if b.Muted && !b.Paused && !b.Silenced {
		return "jobs still run"
	}
	if b.Swallowed == 1 {
		return "1 alert swallowed"
	}
	return fmt.Sprintf("%d alerts swallowed", b.Swallowed)
}

// decideAction is the POST target for a phone decision. It is a same-origin
// path, so templ.SafeURL is honest here.
func decideAction(id int64) templ.SafeURL {
	return templ.SafeURL("/approvals/" + strconv.FormatInt(id, 10) + "/decide")
}

func urlEscape(s string) string { return url.QueryEscape(s) }

// joinArgv renders a job's argv as the one-line command an operator recognizes
// (the read-only preview on the job-schedule modal and the run form) — plain
// whitespace join, the same shape formScheduleArgs expects back on a "shell"
// schedule's argv field.
func joinArgv(argv []string) string { return strings.Join(argv, " ") }

// scheduleAction is the POST target for turning a job into a shell schedule.
func scheduleAction(id int64) templ.SafeURL {
	return templ.SafeURL("/jobs/" + strconv.FormatInt(id, 10) + "/schedule")
}
