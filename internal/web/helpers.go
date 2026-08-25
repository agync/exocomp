package web

import (
	"fmt"
	"net/url"
	"sort"
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

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
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
// (the read-only preview on the job-schedule modal) — plain whitespace join,
// the same shape formScheduleArgs expects back on a "shell" schedule's argv.
func joinArgv(argv []string) string { return strings.Join(argv, " ") }

// scheduleAction is the POST target for turning a job into a shell schedule.
func scheduleAction(id int64) templ.SafeURL {
	return templ.SafeURL("/jobs/" + strconv.FormatInt(id, 10) + "/schedule")
}

// rerunAction is the POST target for resubmitting a finished job with tweaks.
func rerunAction(id int64) templ.SafeURL {
	return templ.SafeURL("/jobs/" + strconv.FormatInt(id, 10) + "/rerun")
}

// extraVarLines renders a job's stored extra-vars as sorted "key=val" lines for
// the rerun modal's read-only preview. Order is stable (sorted by key) so the
// render is deterministic. Only operator-typed vars live here — a sealed secret
// never does (store.JobArgs.ExtraVars) — so showing the value is safe.
func extraVarLines(vars map[string]string) []string {
	if len(vars) == 0 {
		return nil
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+vars[k])
	}
	return out
}

// boolAttr renders a Go bool as the string an ARIA state attribute expects
// ("true"/"false") — distinct from a native boolean attribute (hidden, checked)
// where presence is the state and templ's `attr?={cond}` form is right.
func boolAttr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
