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

// netgroupRow is one named CIDR set on the netgroups surface. Prefixes and
// Excludes are config, not values, so they travel in full. Refusal is the rm
// verb's refusal sentence, carried to the rowActions partial by
// netgroupRowActions (forms/SPEC.md): non-empty only on a refused rm re-render.
type netgroupRow struct {
	Name     string
	Prefixes []string
	Excludes []string
	Refusal  string
}

// Join renders the set as the comma list `netgroup list` shows, the same shape
// the add form accepts back: includes plain, exclusions `!`-prefixed.
func (r netgroupRow) Join() string { return strings.Join(r.members(), ",") }

// Display renders the includes for a table cell: the space-separated · form a
// reader scans, distinct from the comma form the CLI/add form accepts (W16).
func (r netgroupRow) Display() string { return strings.Join(r.Prefixes, " · ") }

// ExcludesDisplay renders the set's exclusions each with their `!` marker, for
// the distinct cell line the panel shows beneath the includes; empty when the
// set excludes nothing.
func (r netgroupRow) ExcludesDisplay() string {
	parts := make([]string, 0, len(r.Excludes))
	for _, e := range r.Excludes {
		parts = append(parts, "!"+e)
	}
	return strings.Join(parts, " · ")
}

// members is the row's set in the comma list order: includes, then `!`-marked
// exclusions.
func (r netgroupRow) members() []string {
	parts := make([]string, 0, len(r.Prefixes)+len(r.Excludes))
	parts = append(parts, r.Prefixes...)
	for _, e := range r.Excludes {
		parts = append(parts, "!"+e)
	}
	return parts
}

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
		for _, p := range g.Excludes {
			row.Excludes = append(row.Excludes, p.String())
		}
		out = append(out, row)
	}
	return out, nil
}
