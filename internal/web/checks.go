package web

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"

	"exocomp.dev/exocomp/internal/store"
)

// checksVM is the /checks surface: named probe sets whose per-target answers
// flip into reactive events on a transition (docs/architecture/reactive.md
// "Checks"). A check carries no secret material, so the write controls here
// are plain operator-scope (checks_write.go), like schedules.
type checksVM struct {
	Page     page
	Operator bool
	Checks   []checkRow
	// Netgroups back the Netgroups tab on /checks (the former /netgroups page,
	// merged per F7); Tab selects which tab renders ("netgroups" or probes).
	Netgroups []netgroupRow
	Tab       string
	// NetgroupForm is the netgroup-add dialog's render state: nil renders the
	// fresh form (help at rest, dialog closed), and a refusal re-renders the
	// surface at 422 with the form filled and the dialog open.
	NetgroupForm *netgroupFormVM
	// Form is the check-add dialog's render state (the same shape): nil at
	// rest, the filled form with field refusals on a 422 re-render.
	Form *checkFormVM
}

// checkRow is one probe check on the checks surface, with each target's last
// observed status from the sweep's check_state rows ("" when the sweep has
// never seen it).
type checkRow struct {
	Name     string
	Kind     string
	Interval string
	Enabled  bool
	Targets  []checkTargetRow
	// Base is the surface's origin sub-path ("" at the root), so the row's
	// Go-built hrefs re-prefix the same way every other URL site does.
	Base string
	// Refusal is the row verb's refusal sentence, carried to the rowActions
	// partial by checkRowActions (forms/SPEC.md): non-empty only on a refused
	// row-verb re-render.
	Refusal string
}

// checkTargetRow is one probe of a check: what it dials or requests, the
// status the sweep last observed, the netgroup the probed address belongs to
// ("" for an http URL target), and how long ago that status last changed (the
// humanized age, "" when the sweep has never seen the target).
type checkTargetRow struct {
	Name     string
	Address  string
	Port     int
	URL      string
	Expect   int
	Status   string
	Netgroup string
	Changed  string
}

// Spec renders the probe location the way the CLI's table reads it: a tcp
// target as address:port, an http target as its URL, a ping target as the
// bare address.
func (t checkTargetRow) Spec() string {
	if t.URL != "" {
		return t.URL
	}
	if t.Port != 0 {
		return net.JoinHostPort(t.Address, strconv.Itoa(t.Port))
	}
	return t.Address
}

// ExpectNote renders an http target's pinned expected status, or "" when the
// target accepts the default 2xx/3xx range.
func (t checkTargetRow) ExpectNote() string {
	if t.Expect == 0 {
		return ""
	}
	return " · expect " + itoa(int64(t.Expect))
}

// StatusClass maps a target's last observed status onto the chip palette: up
// is ok, down is a loud failure, and a target the sweep has never seen is dim.
func (t checkTargetRow) StatusClass() string {
	switch t.Status {
	case store.CheckUp:
		return "ok"
	case store.CheckDown:
		return "fail"
	default:
		return "dim"
	}
}

// StatusLabel names a target's last observed status; the label always names
// the state so the chip is never colour-only.
func (t checkTargetRow) StatusLabel() string {
	switch t.Status {
	case store.CheckUp:
		return "UP"
	case store.CheckDown:
		return "DOWN"
	default:
		return "UNOBSERVED"
	}
}

// IsUp / IsDown / IsUnobserved name the sweep's three verdicts so the template
// branches on the stored constants without importing the store package.
func (t checkTargetRow) IsUp() bool   { return t.Status == store.CheckUp }
func (t checkTargetRow) IsDown() bool { return t.Status == store.CheckDown }
func (t checkTargetRow) IsUnobserved() bool {
	return t.Status != store.CheckUp && t.Status != store.CheckDown
}

// NetgroupClass maps a target's netgroup badge onto the chip palette: a named
// group reads ok (home), a target no group covers reads dim (away). "away" is
// both the miss label and a possible group name — the ambiguity is the
// engine's too, which stamps "away" for a miss, so the page stays consistent
// with the events it describes.
func (t checkTargetRow) NetgroupClass() string {
	if t.Netgroup == "away" {
		return "dim"
	}
	return "ok"
}

// WhereLocation renders the netgroup slot's location word (SPEC §5.1): the
// covered group's name, else "away" — the store's own miss label.
func (t checkTargetRow) WhereLocation() string {
	if t.Netgroup == "" || t.Netgroup == "away" {
		return "away"
	}
	return t.Netgroup
}

// DownSummary is the at-a-glance fail counter the card header carries when any
// target is down: "N of M down" (the tile ring and DOWN chips carry the
// per-target detail). Empty when nothing is down or the check is disabled.
func (c checkRow) DownSummary() string {
	if !c.Enabled {
		return ""
	}
	down := 0
	for _, t := range c.Targets {
		if t.Status == store.CheckDown {
			down++
		}
	}
	if down == 0 {
		return ""
	}
	return itoa(int64(down)) + " of " + itoa(int64(len(c.Targets))) + " down"
}

// handleChecks renders the /checks surface for the session: every probe check
// with each target's posture and location.
func (s *Server) handleChecks(w http.ResponseWriter, r *http.Request) {
	vm, err := s.checksVMFor(r, nil, nil, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, checksList(vm))
}

// checksVMFor builds the checks view model, optionally carrying the
// netgroup-add or check-add dialog's refusal state (nil = fresh form) and a
// refused row verb's state (nil at rest; the refusal renders on the refused
// check or netgroup row, forms/SPEC.md).
func (s *Server) checksVMFor(r *http.Request, ngForm *netgroupFormVM, ckForm *checkFormVM, refusal *rowRefusal) (checksVM, error) {
	rows, err := s.checks(r)
	if err != nil {
		return checksVM{}, err
	}
	ng, err := s.netgroups(r)
	if err != nil {
		return checksVM{}, err
	}
	if refusal != nil {
		for i := range rows {
			if rows[i].Name == refusal.Name {
				rows[i].Refusal = refusal.Msg
			}
		}
		for i := range ng {
			if ng[i].Name == refusal.Name {
				ng[i].Refusal = refusal.Msg
			}
		}
	}
	ident, _ := identityOf(r.Context())
	tab := r.URL.Query().Get("tab")
	if tab != "netgroups" {
		tab = "probes"
	}
	// A refusal re-renders the surface the form lives on, whatever tab the
	// POST arrived under: the netgroup form on the netgroups tab, the check
	// form on the probes tab.
	if ngForm != nil {
		tab = "netgroups"
	}
	if ckForm != nil {
		tab = "probes"
	}
	vm := checksVM{
		Page: s.pageFor(r, "Checks", "checks"), Checks: rows,
		Operator: canWrite(ident), Netgroups: ng, Tab: tab,
		NetgroupForm: ngForm, Form: ckForm,
	}
	if vm.Operator {
		if tab == "netgroups" {
			vm.Page.Add = &pageAdd{Target: "netgroups-add", Label: "netgroup"}
		} else {
			vm.Page.Add = &pageAdd{Target: "checks-add", Label: "check"}
		}
	}
	return vm, nil
}

// checks reads every check (enabled and disabled) with each target's last
// observed status, its netgroup classification, and how long ago that status
// changed, so a target's current posture and location read at a glance. The
// netgroups are loaded once and every address target is classified in memory
// (store.ClassifyNetgroups), never a NetgroupForAddr round-trip per target.
func (s *Server) checks(r *http.Request) ([]checkRow, error) {
	list, err := s.store.ListChecks(r.Context(), false)
	if err != nil {
		return nil, err
	}
	groups, err := s.store.ListNetgroups(r.Context())
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	for _, c := range list {
		for _, t := range c.Targets {
			if t.Address == "" {
				continue
			}
			addr, err := netip.ParseAddr(t.Address)
			if err != nil {
				return nil, fmt.Errorf("check %s target %q address %q does not parse: %w",
					c.Name, t.Name, t.Address, err)
			}
			addrs = append(addrs, addr)
		}
	}
	netgroups := store.ClassifyNetgroups(addrs, groups)

	var out []checkRow
	for _, c := range list {
		row := checkRow{
			Name: c.Name, Kind: c.Kind, Enabled: c.Enabled,
			Interval: store.CheckIntervalLabel(c.IntervalS),
			Base:     s.basePath(),
		}
		states, err := s.store.ListCheckStates(r.Context(), c.ID)
		if err != nil {
			return nil, err
		}
		state := make(map[string]store.CheckState, len(states))
		for _, st := range states {
			state[st.TargetName] = st
		}
		for _, t := range c.Targets {
			tr := checkTargetRow{
				Name: t.Name, Address: t.Address, Port: t.Port,
				URL: t.URL, Expect: t.Expect,
			}
			if st, ok := state[t.Name]; ok {
				tr.Status = st.Status
				tr.Changed = humanAge(s.now().Sub(st.LastChange))
			}
			if t.Address != "" {
				tr.Netgroup = netgroups[netip.MustParseAddr(t.Address).Unmap().String()]
			}
			row.Targets = append(row.Targets, tr)
		}
		out = append(out, row)
	}
	return out, nil
}
