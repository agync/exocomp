package store

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
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

// netgroupNamePattern bounds a group name so it stays a usable label value:
// a rule writes `event.labels["netgroup"] == <name>` in CEL, so anything a
// label key could not hold is refused here.
var netgroupNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

const netgroupColumns = `id, name, prefixes, created_at, updated_at`

// CreateNetgroup inserts a netgroup. A repeated name returns ErrDuplicate.
func (s *Store) CreateNetgroup(ctx context.Context, n NewNetgroup) (Netgroup, error) {
	if !netgroupNamePattern.MatchString(n.Name) {
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

// NetgroupForAddr names the netgroup whose prefix most specifically contains
// addr: the highest Bits() wins, and a tie breaks by name ascending so the
// answer is deterministic however the rows were inserted. A group that is
// registered but does not contain addr is not a match. It reports false when no
// registered group contains addr.
func (s *Store) NetgroupForAddr(ctx context.Context, addr netip.Addr) (string, bool) {
	addr = addr.Unmap()

	groups, err := s.ListNetgroups(ctx)
	if err != nil {
		return "", false
	}
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
	return best, best != ""
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
