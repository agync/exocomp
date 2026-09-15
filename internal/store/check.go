package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"exocomp.dev/exocomp/internal/probe"
)

// Check kinds.
const (
	CheckTCP    = "tcp"
	CheckPing   = "ping"
	CheckHTTP   = "http"
	CheckDNS    = "dns"
	CheckTLS    = "tls"
	CheckMetric = "metric"
)

// The statuses a check_state row carries. They are probe results, not event
// statuses: the sweep maps them onto firing/resolved at emission.
const (
	CheckUp   = "up"
	CheckDown = "down"
)

// namePattern bounds a check's name, a netgroup's name, and each target's name
// so they all stay usable host and label values: a rule writes
// `event.host == <target>` and reads labels, so anything those could not hold
// is refused here. ValidName is the single gate every store uses.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ValidName reports whether a name stays usable as a host and a label value.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// CheckTarget is one probe of a check. Which fields are set depends on kind:
// tcp uses Address+Port, ping uses Address only, http uses URL (with an
// optional Expect status), dns uses DNSName (with an optional ExpectAddr),
// tls uses Address+Port (with an optional MinDays floor), and metric uses URL
// (with Op and Value). The new kinds' fields are additive, so a targets_json
// written before them still decodes.
type CheckTarget struct {
	Name       string  `json:"name"`
	Address    string  `json:"address,omitempty"`
	Port       int     `json:"port,omitempty"`
	URL        string  `json:"url,omitempty"`
	Expect     int     `json:"expect,omitempty"`
	DNSName    string  `json:"dns_name,omitempty"`
	ExpectAddr string  `json:"expect_addr,omitempty"`
	MinDays    int     `json:"min_days,omitempty"`
	Op         string  `json:"op,omitempty"`
	Value      float64 `json:"value,omitempty"`
}

// ParseCheckTargets splits each name=spec target by kind — the shape both
// `check add`'s --target flags and the checks page's repeated target fields
// submit. The spec after the '=' is an IP literal (tcp/ping) or a URL (http);
// a tcp target carries its port after the last ':'. skipBlank is the web
// form's leniency: an unused row of the form is trimmed and skipped, where the
// CLI's strict mode refuses it.
func ParseCheckTargets(kind string, specs []string, skipBlank bool) ([]CheckTarget, error) {
	out := make([]CheckTarget, 0, len(specs))
	for _, spec := range specs {
		if skipBlank {
			spec = strings.TrimSpace(spec)
			if spec == "" {
				continue
			}
		}
		name, value, ok := strings.Cut(spec, "=")
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("target %q must be name=spec", spec)
		}
		t := CheckTarget{Name: name}
		switch kind {
		case CheckTCP:
			host, port, err := net.SplitHostPort(value)
			if err != nil {
				return nil, fmt.Errorf("target %q: a tcp target must be ip:port", spec)
			}
			p, err := strconv.Atoi(port)
			if err != nil {
				return nil, fmt.Errorf("target %q: port %q is not a number", spec, port)
			}
			t.Address = host
			t.Port = p
		case CheckPing:
			t.Address = value
		case CheckHTTP:
			t.URL = value
		case CheckDNS:
			t.DNSName = value
		case CheckTLS:
			host, port, err := net.SplitHostPort(value)
			if err != nil {
				return nil, fmt.Errorf("target %q: a tls target must be host:port", spec)
			}
			p, err := strconv.Atoi(port)
			if err != nil {
				return nil, fmt.Errorf("target %q: port %q is not a number", spec, port)
			}
			t.Address = host
			t.Port = p
		case CheckMetric:
			// Op and Value are unset here: a surface that adds a metric target
			// supplies them, and validation refuses a metric target until it
			// does, so a half-declared threshold can never be stored.
			t.URL = value
		default:
			return nil, fmt.Errorf("--kind must be one of %s, %s, %s, %s, %s, %s",
				CheckTCP, CheckPing, CheckHTTP, CheckDNS, CheckTLS, CheckMetric)
		}
		out = append(out, t)
	}
	return out, nil
}

// Check is one row of the check table.
type Check struct {
	ID        int64
	Name      string
	Kind      string
	IntervalS int64
	Targets   []CheckTarget
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
	// SnoozedUntil is when a check-level snooze expires. The zero time is not
	// snoozed. The sweep keeps probing while snoozed so hysteresis stays
	// current; it only suppresses transition emission.
	SnoozedUntil time.Time
}

// NewCheck describes a check to create. It starts enabled.
type NewCheck struct {
	Name      string
	Kind      string
	IntervalS int64
	Targets   []CheckTarget
}

const checkColumns = `id, name, kind, interval_s, targets_json, enabled, created_at, updated_at, snoozed_until`

// CreateCheck inserts an enabled check. Names are unique. The per-kind target
// shape is validated here so a row that reads is a row that probes: tcp and
// ping target addresses are IP literals (the address is the location), http
// targets are http(s) URLs the probe will dial.
func (s *Store) CreateCheck(ctx context.Context, n NewCheck) (Check, error) {
	if !ValidName(n.Name) {
		return Check{}, fmt.Errorf("%w: store: check name %q must match [A-Za-z0-9_.-]+", ErrValidation, n.Name)
	}
	switch n.Kind {
	case CheckTCP, CheckPing, CheckHTTP, CheckDNS, CheckTLS, CheckMetric:
	default:
		return Check{}, fmt.Errorf("%w: store: check kind %q must be one of %s, %s, %s, %s, %s, %s",
			ErrValidation, n.Kind, CheckTCP, CheckPing, CheckHTTP, CheckDNS, CheckTLS, CheckMetric)
	}
	if n.IntervalS <= 0 {
		return Check{}, fmt.Errorf("%w: store: check %q interval_s must be positive, got %d", ErrValidation, n.Name, n.IntervalS)
	}
	if err := validateCheckTargets(n.Name, n.Kind, n.Targets); err != nil {
		return Check{}, err
	}

	targets, err := json.Marshal(n.Targets)
	if err != nil {
		return Check{}, fmt.Errorf("store: create check: encode targets: %w", err)
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO check_row (name, kind, interval_s, targets_json, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)`,
		n.Name, n.Kind, n.IntervalS, string(targets), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return Check{}, wrap("create check", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Check{}, wrap("create check", err)
	}
	return s.getCheck(ctx, id)
}

// UpdateCheck re-points a stored check's targets and interval in place. The
// kind is read from the stored row and never replaced; the target shape rules
// are the same CreateCheck enforces, so a row that reads is a row that probes.
// check_state rows key on (check_id, target_name), so a re-point that keeps a
// target's NAME preserves its state — the sweep can flip the kept row and emit
// a real transition. That is the whole point of `check update` over rm+add,
// which cascades the state away.
func (s *Store) UpdateCheck(ctx context.Context, name string, targets []CheckTarget, intervalS int64) (Check, error) {
	cur, err := s.GetCheck(ctx, name)
	if err != nil {
		return Check{}, err
	}
	if intervalS <= 0 {
		return Check{}, fmt.Errorf("%w: store: check %q interval_s must be positive, got %d", ErrValidation, name, intervalS)
	}
	if err := validateCheckTargets(name, cur.Kind, targets); err != nil {
		return Check{}, err
	}

	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		return Check{}, fmt.Errorf("store: update check: encode targets: %w", err)
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE check_row SET targets_json = ?, interval_s = ?, updated_at = ?
		WHERE id = ?`,
		string(targetsJSON), intervalS, now.UnixMilli(), cur.ID); err != nil {
		return Check{}, wrap("update check", err)
	}
	// A re-point drops targets the new set no longer names; their state rows
	// must go with them, or the metrics and any state-row reader keep reporting
	// a target the check no longer probes. A kept NAME keeps its row, which is
	// what lets the sweep flip it and emit a real transition.
	keep := make(map[string]bool, len(targets))
	for _, t := range targets {
		keep[t.Name] = true
	}
	states, err := s.ListCheckStates(ctx, cur.ID)
	if err != nil {
		return Check{}, err
	}
	for _, st := range states {
		if keep[st.TargetName] {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM check_state WHERE check_id = ? AND target_name = ?`,
			cur.ID, st.TargetName); err != nil {
			return Check{}, wrap("update check: prune state", err)
		}
	}
	return s.getCheck(ctx, cur.ID)
}

// validateCheckTargets enforces the per-kind target shape CreateCheck and
// UpdateCheck share, so an update's refusal rules can never drift from a
// create's. The ping kind's capability probe (binary on PATH AND a live
// loopback answer) re-runs on a ping update too: validate-at-write is
// deliberate, a check that is silently all-down is the failure it prevents.
func validateCheckTargets(name, kind string, targets []CheckTarget) error {
	if len(targets) == 0 {
		return fmt.Errorf("%w: store: check %q needs at least one target", ErrValidation, name)
	}
	seen := make(map[string]bool, len(targets))
	for i, t := range targets {
		if !ValidName(t.Name) {
			return fmt.Errorf("%w: store: check %q target %d name %q must match [A-Za-z0-9_.-]+",
				ErrValidation, name, i, t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("%w: store: check %q has a duplicated target name %q", ErrValidation, name, t.Name)
		}
		seen[t.Name] = true

		switch kind {
		case CheckTCP:
			if _, err := netip.ParseAddr(t.Address); err != nil {
				return fmt.Errorf("%w: store: check %q target %q address %q is not an IP literal: %w",
					ErrValidation, name, t.Name, t.Address, err)
			}
			if t.Port < 1 || t.Port > 65535 {
				return fmt.Errorf("%w: store: check %q target %q port %d out of range 1..65535",
					ErrValidation, name, t.Name, t.Port)
			}
		case CheckPing:
			if _, err := netip.ParseAddr(t.Address); err != nil {
				return fmt.Errorf("%w: store: check %q target %q address %q is not an IP literal: %w",
					ErrValidation, name, t.Name, t.Address, err)
			}
			if t.Port != 0 {
				return fmt.Errorf("%w: store: check %q target %q carries a port; ping probes an address",
					ErrValidation, name, t.Name)
			}
			// Rootless fail-closed: a binary on PATH alone does not mean the
			// host can send ICMP (ping_group_range/setuid), so the kind is
			// refused unless the binary exists AND a live loopback probe
			// answers. A check that is silently all-down is the observability
			// failure this refusal exists to prevent.
			if err := pingCapable(); err != nil {
				return fmt.Errorf("store: check %q kind ping: %w", name, err)
			}
		case CheckHTTP:
			u, err := url.Parse(t.URL)
			if err != nil {
				return fmt.Errorf("%w: store: check %q target %q url %q does not parse: %w",
					ErrValidation, name, t.Name, t.URL, err)
			}
			if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("%w: store: check %q target %q url %q must be an http(s) URL with a host",
					ErrValidation, name, t.Name, t.URL)
			}
			if t.Expect != 0 && (t.Expect < 100 || t.Expect > 599) {
				return fmt.Errorf("%w: store: check %q target %q expect %d out of range 100..599",
					ErrValidation, name, t.Name, t.Expect)
			}
		case CheckDNS, CheckTLS, CheckMetric:
			// The new kinds share the probe package's per-kind rules, so a
			// check's write validation and the runner cannot drift.
			spec, err := SpecForTarget(kind, t)
			if err != nil {
				return fmt.Errorf("%w: store: check %q target %q: %v", ErrValidation, name, t.Name, err)
			}
			if err := spec.Validate(); err != nil {
				return fmt.Errorf("%w: store: check %q target %q: %v", ErrValidation, name, t.Name, err)
			}
		}
	}
	return nil
}

// SpecForTarget maps a stored check kind and target to the shared probe.Spec
// its runner builds. It is the one place the per-kind field mapping lives, so
// validation and execution cannot disagree — a tls target's host:port join, a
// check http's 2xx/3xx default, and the new kinds' optional fields are decided
// here once.
func SpecForTarget(kind string, t CheckTarget) (probe.Spec, error) {
	switch kind {
	case CheckTCP:
		return probe.Spec{Kind: probe.KindTCP, Address: t.Address, Port: t.Port}, nil
	case CheckPing:
		return probe.Spec{Kind: probe.KindPing, Address: t.Address}, nil
	case CheckHTTP:
		return probe.Spec{Kind: probe.KindHTTP, URL: t.URL, ExpectStatus: t.Expect, HTTPAllow3xx: true}, nil
	case CheckDNS:
		return probe.Spec{Kind: probe.KindDNS, DNSName: t.DNSName, ExpectAddr: t.ExpectAddr}, nil
	case CheckTLS:
		return probe.Spec{Kind: probe.KindTLS, Address: net.JoinHostPort(t.Address, strconv.Itoa(t.Port)), MinDays: t.MinDays}, nil
	case CheckMetric:
		return probe.Spec{Kind: probe.KindMetric, URL: t.URL, Op: t.Op, Value: t.Value}, nil
	default:
		return probe.Spec{}, fmt.Errorf("unknown check kind %q", kind)
	}
}

// getCheck returns the check with the given id.
func (s *Store) getCheck(ctx context.Context, id int64) (Check, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+checkColumns+` FROM check_row WHERE id = ?`, id)
	c, err := scanCheck(row)
	if err != nil {
		return Check{}, wrap("get check", err)
	}
	return c, nil
}

// GetCheck returns the check with the given name.
func (s *Store) GetCheck(ctx context.Context, name string) (Check, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+checkColumns+` FROM check_row WHERE name = ?`, name)
	c, err := scanCheck(row)
	if err != nil {
		return Check{}, wrap("get check", err)
	}
	return c, nil
}

// ListChecks returns checks ordered by name; enabledOnly narrows to the checks
// the sweep probes.
func (s *Store) ListChecks(ctx context.Context, enabledOnly bool) ([]Check, error) {
	query := `SELECT ` + checkColumns + ` FROM check_row`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY name`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, wrap("list checks", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Check
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, wrap("list checks", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list checks", err)
	}
	return out, nil
}

// SetCheckEnabled flips a check's enabled flag by name. A disabled check stays
// in the table (and its state) but ListChecks(true) — and so the sweep — skip
// it.
func (s *Store) SetCheckEnabled(ctx context.Context, name string, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE check_row SET enabled = ? WHERE name = ?`, enabled, name)
	if err != nil {
		return wrap("set check enabled", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set check enabled", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set check enabled %q: %w", name, ErrNotFound)
	}
	return nil
}

// SetCheckSnooze sets a check's snooze expiry by name. A time at or before now
// clears it (as does the zero time). A snoozed check is still probed and its
// state still advances; only transition emission is suppressed until the
// expiry passes.
func (s *Store) SetCheckSnooze(ctx context.Context, name string, until time.Time) error {
	ms := int64(0)
	if !until.IsZero() {
		ms = until.UTC().UnixMilli()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE check_row SET snoozed_until = ? WHERE name = ?`, ms, name)
	if err != nil {
		return wrap("set check snooze", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set check snooze", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set check snooze %q: %w", name, ErrNotFound)
	}
	return nil
}

// DeleteCheck removes a check by name, and with it its state rows (the FK
// cascades).
func (s *Store) DeleteCheck(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM check_row WHERE name = ?`, name)
	if err != nil {
		return wrap("delete check", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete check", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete check %q: %w", name, ErrNotFound)
	}
	return nil
}

// CheckState is one row of check_state: where a target's status currently
// stands, how many consecutive observations it has held, and the last status
// that was emitted as an event. A missing row is the unobserved cold-start
// state; the sweep seeds one on first observation without emitting.
type CheckState struct {
	CheckID     int64
	TargetName  string
	Status      string
	Consecutive int
	Emitted     string
	FirstSeen   time.Time
	LastChange  time.Time
	// RecentObservations is the target's bounded observation ring (JSON):
	// {observed_at, status, consecutive} per probe, newest last, trimmed to the
	// hysteresis window. The sweep appends each observation and embeds the
	// ring's tail into the event a transition raises, so the "WHY THIS COUNTED
	// AS A TRANSITION" ledger is exact, not reconstructed.
	RecentObservations string
	// Netgroup is the netgroup the target's address classified into at the last
	// observation ("away" when none covered it, "" for an http URL target). The
	// sweep stores it so every read path reads the same value.
	Netgroup string
	// LastDurationMS is how long the last probe took, in milliseconds. It is
	// observation output, not configuration: a slow-but-up target is visible
	// without a second probe.
	LastDurationMS int64
}

const checkStateColumns = `check_id, target_name, status, consecutive, emitted, first_seen, last_change, recent_observations, netgroup, last_duration_ms`

// GetCheckState returns one target's state, or ErrNotFound.
func (s *Store) GetCheckState(ctx context.Context, checkID int64, target string) (CheckState, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+checkStateColumns+` FROM check_state WHERE check_id = ? AND target_name = ?`,
		checkID, target)
	st, err := scanCheckState(row)
	if err != nil {
		return CheckState{}, wrap("get check state", err)
	}
	return st, nil
}

// ListCheckStates returns every target's state for a check, by target name.
func (s *Store) ListCheckStates(ctx context.Context, checkID int64) ([]CheckState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+checkStateColumns+` FROM check_state WHERE check_id = ? ORDER BY target_name`,
		checkID)
	if err != nil {
		return nil, wrap("list check states", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CheckState
	for rows.Next() {
		st, err := scanCheckState(rows)
		if err != nil {
			return nil, wrap("list check states", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list check states", err)
	}
	return out, nil
}

// CountCheckStatesByStatus counts the targets whose state row currently holds
// status — the dashboard's "N targets up" card line. A COUNT over check_state
// rather than a per-check ListCheckStates loop: the glance screen must never
// pull every state row into memory just to size a number. It joins check_row
// on enabled=1, so only the targets the sweep currently probes count: a
// disabled check's frozen state rows are not probed and would overstate the
// card.
func (s *Store) CountCheckStatesByStatus(ctx context.Context, status string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM check_state
		 JOIN check_row ON check_row.id = check_state.check_id
		 WHERE check_state.status = ? AND check_row.enabled = 1`, status).Scan(&n)
	if err != nil {
		return 0, wrap("count check states", err)
	}
	return n, nil
}

// SetCheckState upserts one target's state. first_seen is written on insert
// and preserved on update, so it stays the target's first observation for the
// check's life.
func (s *Store) SetCheckState(ctx context.Context, st CheckState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO check_state (check_id, target_name, status, consecutive, emitted, first_seen, last_change, recent_observations, netgroup, last_duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (check_id, target_name) DO UPDATE SET
			status              = excluded.status,
			consecutive         = excluded.consecutive,
			emitted             = excluded.emitted,
			last_change         = excluded.last_change,
			recent_observations = excluded.recent_observations,
			netgroup            = excluded.netgroup,
			last_duration_ms    = excluded.last_duration_ms`,
		st.CheckID, st.TargetName, st.Status, st.Consecutive, st.Emitted,
		st.FirstSeen.UTC().UnixMilli(), st.LastChange.UTC().UnixMilli(),
		st.RecentObservations, st.Netgroup, st.LastDurationMS)
	if err != nil {
		return wrap("set check state", err)
	}
	return nil
}

// CheckIntervalLabel renders a stored interval as the Go duration string
// `check list` and the checks page share, so the CLI and the browser read a
// check's cadence identically.
func CheckIntervalLabel(intervalS int64) string {
	if intervalS <= 0 {
		return "-"
	}
	return (time.Duration(intervalS) * time.Second).String()
}

func scanCheck(row rowScanner) (Check, error) {
	var (
		c            Check
		targets      string
		enabled      int
		createdAt    int64
		updatedAt    int64
		snoozedUntil int64
	)
	if err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.IntervalS, &targets,
		&enabled, &createdAt, &updatedAt, &snoozedUntil); err != nil {
		return Check{}, err
	}
	c.Enabled = enabled != 0
	c.CreatedAt = time.UnixMilli(createdAt).UTC()
	c.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	if snoozedUntil != 0 {
		c.SnoozedUntil = time.UnixMilli(snoozedUntil).UTC()
	}
	if targets != "" {
		if err := json.Unmarshal([]byte(targets), &c.Targets); err != nil {
			return Check{}, fmt.Errorf("decode targets of check %d: %w", c.ID, err)
		}
	}
	return c, nil
}

func scanCheckState(row rowScanner) (CheckState, error) {
	var (
		st          CheckState
		consecutive int64
		firstSeen   int64
		lastChange  int64
	)
	if err := row.Scan(&st.CheckID, &st.TargetName, &st.Status, &consecutive,
		&st.Emitted, &firstSeen, &lastChange, &st.RecentObservations, &st.Netgroup,
		&st.LastDurationMS); err != nil {
		return CheckState{}, err
	}
	st.Consecutive = int(consecutive)
	st.FirstSeen = time.UnixMilli(firstSeen).UTC()
	st.LastChange = time.UnixMilli(lastChange).UTC()
	return st, nil
}

// pingCapable reports whether this host can actually send ICMP: the binary
// exists AND a loopback probe answers. PATH presence alone is not evidence
// (rootless ICMP needs net.ipv4.ping_group_range or a setuid helper).
//
// The probe runs from the store on purpose (validate-at-write, deliberate):
// the refusal must happen where the check is written, before a row that can
// never be probed exists. ping is a pinned system binary, not a store
// dependency.
func pingCapable() error {
	if _, err := exec.LookPath("ping"); err != nil {
		return fmt.Errorf("ping binary not on PATH: %w", err)
	}
	// -W 2 bounds the probe so a wedged ICMP path fails fast instead of
	// hanging `check add`.
	if err := exec.CommandContext(context.Background(), "ping", "-c", "1", "-W", "2", "127.0.0.1").Run(); err != nil {
		return fmt.Errorf("loopback ping probe failed: %w", err)
	}
	return nil
}
