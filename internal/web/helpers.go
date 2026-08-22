package web

import (
	"fmt"
	"net/url"
	"strconv"

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
