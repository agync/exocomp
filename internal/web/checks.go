package web

import (
	"net"
	"net/http"
	"strconv"
	"time"

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
}

// checkTargetRow is one probe of a check: what it dials or requests, and the
// status the sweep last observed.
type checkTargetRow struct {
	Name    string
	Address string
	Port    int
	URL     string
	Expect  int
	Status  string
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

func (s *Server) handleChecks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.checks(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ident, _ := identityOf(r.Context())
	vm := checksVM{
		Page: s.pageFor(r, "Checks", "checks"), Checks: rows,
		Operator: canWrite(ident),
	}
	s.render(w, r, http.StatusOK, checksList(vm))
}

// checks reads every check (enabled and disabled) with each target's last
// observed status, so a target's current posture reads at a glance.
func (s *Server) checks(r *http.Request) ([]checkRow, error) {
	list, err := s.store.ListChecks(r.Context(), false)
	if err != nil {
		return nil, err
	}
	var out []checkRow
	for _, c := range list {
		row := checkRow{
			Name: c.Name, Kind: c.Kind, Enabled: c.Enabled,
			Interval: checkIntervalLabel(c.IntervalS),
		}
		states, err := s.store.ListCheckStates(r.Context(), c.ID)
		if err != nil {
			return nil, err
		}
		status := make(map[string]string, len(states))
		for _, st := range states {
			status[st.TargetName] = st.Status
		}
		for _, t := range c.Targets {
			row.Targets = append(row.Targets, checkTargetRow{
				Name: t.Name, Address: t.Address, Port: t.Port,
				URL: t.URL, Expect: t.Expect, Status: status[t.Name],
			})
		}
		out = append(out, row)
	}
	return out, nil
}

// checkIntervalLabel renders a stored interval as the Go duration string the
// CLI shows, so the page and `check list` agree on how a check's cadence
// reads.
func checkIntervalLabel(intervalS int64) string {
	if intervalS <= 0 {
		return "-"
	}
	return (time.Duration(intervalS) * time.Second).String()
}
