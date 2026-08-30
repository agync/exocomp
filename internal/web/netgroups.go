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
// config, not values, so they travel in full.
type netgroupRow struct {
	Name     string
	Prefixes []string
}

// Join renders the prefixes as the comma list `netgroup list` shows, the same
// shape the add form accepts back.
func (r netgroupRow) Join() string { return strings.Join(r.Prefixes, ",") }

func (s *Server) handleNetgroups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.netgroups(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ident, _ := identityOf(r.Context())
	vm := netgroupsVM{
		Page: s.pageFor(r, "Netgroups", "netgroups"), Netgroups: rows,
		Operator: canWrite(ident),
	}
	s.render(w, r, http.StatusOK, netgroupsList(vm))
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
