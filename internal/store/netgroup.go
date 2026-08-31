package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Netgroup is one named CIDR set. It classifies an address — a webhook peer or
// a check target — with the operator's vocabulary, so a rule can say
// "home" instead of hardcoding the prefixes the address has to fall in.
type Netgroup struct {
	ID        int64
	Name      string
	Prefixes  []netip.Prefix
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewNetgroup describes a netgroup to create.
type NewNetgroup struct {
	Name     string
	Prefixes []netip.Prefix
}

const netgroupColumns = `id, name, prefixes, created_at, updated_at`

// CreateNetgroup inserts a netgroup. A repeated name returns ErrDuplicate.
func (s *Store) CreateNetgroup(ctx context.Context, n NewNetgroup) (Netgroup, error) {
	if !ValidName(n.Name) {
		return Netgroup{}, fmt.Errorf("store: netgroup name %q must match [A-Za-z0-9_.-]+", n.Name)
	}
	if len(n.Prefixes) == 0 {
		return Netgroup{}, fmt.Errorf("store: netgroup %q needs at least one prefix", n.Name)
	}
	for i, p := range n.Prefixes {
		if !p.IsValid() {
			return Netgroup{}, fmt.Errorf("store: netgroup %q prefix %d is not a valid CIDR", n.Name, i)
		}
	}

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO netgroup (name, prefixes, created_at, updated_at)
		VALUES (?, ?, ?, ?)`,
		n.Name, encodePrefixes(n.Prefixes), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return Netgroup{}, wrap("create netgroup", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Netgroup{}, wrap("create netgroup", err)
	}
	return s.getNetgroup(ctx, id)
}

func (s *Store) getNetgroup(ctx context.Context, id int64) (Netgroup, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+netgroupColumns+` FROM netgroup WHERE id = ?`, id)
	g, err := scanNetgroup(row)
	if err != nil {
		return Netgroup{}, wrap("get netgroup", err)
	}
	return g, nil
}

// GetNetgroup returns the netgroup with the given name, or ErrNotFound.
func (s *Store) GetNetgroup(ctx context.Context, name string) (Netgroup, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+netgroupColumns+` FROM netgroup WHERE name = ?`, name)
	g, err := scanNetgroup(row)
	if err != nil {
		return Netgroup{}, wrap("get netgroup", err)
	}
	return g, nil
}

// ListNetgroups returns every netgroup by name.
func (s *Store) ListNetgroups(ctx context.Context) ([]Netgroup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+netgroupColumns+` FROM netgroup ORDER BY name`)
	if err != nil {
		return nil, wrap("list netgroups", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Netgroup
	for rows.Next() {
		g, err := scanNetgroup(rows)
		if err != nil {
			return nil, wrap("list netgroups", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list netgroups", err)
	}
	return out, nil
}

// DeleteNetgroup removes a netgroup by name.
func (s *Store) DeleteNetgroup(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM netgroup WHERE name = ?`, name)
	if err != nil {
		return wrap("delete netgroup", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete netgroup", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete netgroup %q: %w", name, ErrNotFound)
	}
	return nil
}

// ParsePrefixList splits a comma-separated CIDR string and parses every member
// — the shape both `netgroup add`'s positional argument and the netgroups
// page's prefixes field submit. An empty member is refused too: in the middle
// of a hand-typed list it is a double comma, a typo that should be caught here
// rather than stored.
func ParsePrefixList(spec string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("an empty CIDR (double comma) in the list")
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR: %w", part, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// NetgroupForAddr names the netgroup whose prefix most specifically contains
// addr: the highest Bits() wins, and a tie breaks by name ascending so the
// answer is deterministic however the rows were inserted. A group that is
// registered but does not contain addr is not a match. It returns ErrNotFound
// when no registered group contains addr, and the error when the lookup fails
// so the caller can tell a miss from a broken read.
func (s *Store) NetgroupForAddr(ctx context.Context, addr netip.Addr) (string, error) {
	groups, err := s.ListNetgroups(ctx)
	if err != nil {
		return "", err
	}
	name, ok := classifyAddr(addr, groups)
	if !ok {
		return "", ErrNotFound
	}
	return name, nil
}

// classifyAddr returns the netgroup whose prefix most specifically contains
// addr, or ok=false when none does. It is the in-memory classifier both
// NetgroupForAddr and ClassifyNetgroups share.
func classifyAddr(addr netip.Addr, groups []Netgroup) (string, bool) {
	addr = addr.Unmap()
	best := ""
	bestBits := -1
	for _, g := range groups {
		for _, p := range g.Prefixes {
			if !p.Contains(addr) {
				continue
			}
			if p.Bits() > bestBits || (p.Bits() == bestBits && (best == "" || g.Name < best)) {
				best, bestBits = g.Name, p.Bits()
			}
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// ClassifyNetgroups classifies every address in one in-memory pass, returning
// the group each one most specifically falls in keyed by the address's
// canonical (unmapped) string. An address no registered group covers reads
// "away", the same miss label the engine stamps on an event. Callers that
// already hold the netgroups load them once and classify all their targets,
// instead of a NetgroupForAddr round-trip per target.
func ClassifyNetgroups(addrs []netip.Addr, groups []Netgroup) map[string]string {
	out := make(map[string]string, len(addrs))
	for _, addr := range addrs {
		addr = addr.Unmap()
		if name, ok := classifyAddr(addr, groups); ok {
			out[addr.String()] = name
		} else {
			out[addr.String()] = "away"
		}
	}
	return out
}

func scanNetgroup(row rowScanner) (Netgroup, error) {
	var (
		g                    Netgroup
		prefixes             string
		createdAt, updatedAt int64
	)
	if err := row.Scan(&g.ID, &g.Name, &prefixes, &createdAt, &updatedAt); err != nil {
		return Netgroup{}, err
	}
	parsed, err := decodePrefixes(prefixes)
	if err != nil {
		return Netgroup{}, err
	}
	g.Prefixes = parsed
	g.CreatedAt = time.UnixMilli(createdAt).UTC()
	g.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return g, nil
}

// encodePrefixes joins prefixes comma-separated for the prefixes column. The
// column holds plain text only because it is never queried structurally; the
// lookups parse every row, which is fine at the handful-of-groups scale a
// single operator keeps.
func encodePrefixes(prefixes []netip.Prefix) string {
	parts := make([]string, len(prefixes))
	for i, p := range prefixes {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}

// decodePrefixes splits a prefixes column back into parsed prefixes. A row
// that reads is a row that parses: CreateNetgroup validated every prefix
// before storing it.
func decodePrefixes(s string) ([]netip.Prefix, error) {
	if s == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("store: netgroup prefix %q does not parse: %w", part, err)
		}
		out = append(out, p)
	}
	return out, nil
}
