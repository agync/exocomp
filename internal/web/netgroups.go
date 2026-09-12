package web

import (
	"net/http"
	"strings"
)

// netgroupsVM is the /netgroups surface: named CIDR sets that give the
// operator's own vocabulary to address classification — a webhook peer or a
// check target — so a rule writes `event.labels["netgroup"] == "home"`
// instead of a prefix literal. A netgroup carries no secret material, so the
// write controls here are plain operator-scope (netgroups_write.go), like
// schedules.
type netgroupsVM struct {
	Page      page
	Operator  bool
	Netgroups []netgroupRow
}

// netgroupRow is one named CIDR set on the netgroups surface. Prefixes are
// config, not values, so they travel in full. Refusal is the rm verb's refusal
// sentence, carried to the rowActions partial by netgroupRowActions
// (forms/SPEC.md): non-empty only on a refused rm re-render.
type netgroupRow struct {
	Name     string
	Prefixes []string
	Refusal  string
}

// Join renders the prefixes as the comma list `netgroup list` shows, the same
// shape the add form accepts back.
func (r netgroupRow) Join() string { return strings.Join(r.Prefixes, ",") }

// Display renders the prefixes for a table cell: the space-separated · form a
// reader scans, distinct from the comma form the CLI/add form accepts (W16).
func (r netgroupRow) Display() string { return strings.Join(r.Prefixes, " · ") }

// handleNetgroups is the merge redirect (F7): netgroups live as a tab on
// /checks, so the standalone page points there. The write routes
// (netgroups_write.go) repoint the same way; the API route stays.
func (s *Server) handleNetgroups(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.url("/checks?tab=netgroups"), http.StatusFound)
}

// netgroups reads every registered CIDR set into rows.
func (s *Server) netgroups(r *http.Request) ([]netgroupRow, error) {
	list, err := s.store.ListNetgroups(r.Context())
	if err != nil {
		return nil, err
	}
	var out []netgroupRow
	for _, g := range list {
		row := netgroupRow{Name: g.Name}
		for _, p := range g.Prefixes {
			row.Prefixes = append(row.Prefixes, p.String())
		}
		out = append(out, row)
	}
	return out, nil
}
