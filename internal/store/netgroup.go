package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// NetgroupAway is the miss label: the value stamped when no registered group
// covers an address, and the one name a netgroup may not take because it is
// byte-identical to that label.
const NetgroupAway = "away"

// Netgroup is one named CIDR set. It classifies an address — a webhook peer or
// a check target — with the operator's vocabulary, so a rule can say
// "home" instead of hardcoding the prefixes the address has to fall in. The
// set may carry exclusions: an address inside an exclusion matches nothing.
type Netgroup struct {
	ID        int64
	Name      string
	Prefixes  []netip.Prefix
	Excludes  []netip.Prefix
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewNetgroup describes a netgroup to create. Prefixes are the includes;
// Excludes are the `!`-marked members.
type NewNetgroup struct {
	Name     string
	Prefixes []netip.Prefix
	Excludes []netip.Prefix
}

const netgroupColumns = `id, name, prefixes, created_at, updated_at`

// CreateNetgroup inserts a netgroup. A repeated name returns ErrDuplicate.
func (s *Store) CreateNetgroup(ctx context.Context, n NewNetgroup) (Netgroup, error) {
	if err := validateNetgroup(n.Name, n.Prefixes, n.Excludes); err != nil {
		return Netgroup{}, err
	}

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO netgroup (name, prefixes, created_at, updated_at)
		VALUES (?, ?, ?, ?)`,
		n.Name, encodePrefixes(n.Prefixes, n.Excludes), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return Netgroup{}, wrap("create netgroup", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Netgroup{}, wrap("create netgroup", err)
	}
	return s.getNetgroup(ctx, id)
}

// UpdateNetgroup replaces a group's includes and excludes in place, preserving
// the row's id and created_at and bumping updated_at. Only the netgroup row
// moves: check_state's stored classification is observation output the next
// sweep refreshes, so an update never disturbs it. An unknown name returns
// ErrNotFound.
func (s *Store) UpdateNetgroup(ctx context.Context, name string, includes, excludes []netip.Prefix) (Netgroup, error) {
	if err := validateNetgroup(name, includes, excludes); err != nil {
		return Netgroup{}, err
	}

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE netgroup SET prefixes = ?, updated_at = ? WHERE name = ?`,
		encodePrefixes(includes, excludes), now.UnixMilli(), name)
	if err != nil {
		return Netgroup{}, wrap("update netgroup", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Netgroup{}, wrap("update netgroup", err)
	}
	if affected == 0 {
		return Netgroup{}, fmt.Errorf("store: update netgroup %q: %w", name, ErrNotFound)
	}
	return s.GetNetgroup(ctx, name)
}

// validateNetgroup holds the shape every write shares: a label-safe name that
// is not the miss label, at least one include, and only valid prefixes.
func validateNetgroup(name string, includes, excludes []netip.Prefix) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: store: netgroup name %q must match [A-Za-z0-9_.-]+", ErrValidation, name)
	}
	if name == NetgroupAway {
		return fmt.Errorf("%w: store: netgroup name %q is reserved: it is the miss label", ErrValidation, name)
	}
	if len(includes) == 0 {
		return fmt.Errorf("%w: store: netgroup %q needs at least one prefix", ErrValidation, name)
	}
	for i, p := range includes {
		if !p.IsValid() {
			return fmt.Errorf("%w: store: netgroup %q prefix %d is not a valid CIDR", ErrValidation, name, i)
		}
	}
	for i, p := range excludes {
		if !p.IsValid() {
			return fmt.Errorf("%w: store: netgroup %q exclude %d is not a valid CIDR", ErrValidation, name, i)
		}
	}
	return nil
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

// PrefixList is a parsed CIDR list split into the includes and the `!`-marked
// excludes the prefixes column stores.
type PrefixList struct {
	Includes []netip.Prefix
	Excludes []netip.Prefix
}

// ParsePrefixList splits a comma-separated CIDR string and parses every member
// — the shape both `netgroup add`'s positional argument and the netgroups
// page's prefixes field submit. A member prefixed with `!` is an exclusion. An
// empty member is refused too: in the middle of a hand-typed list it is a
// double comma, a typo that should be caught here rather than stored.
func ParsePrefixList(spec string) (PrefixList, error) {
	var out PrefixList
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		raw := part
		exclude := strings.HasPrefix(part, "!")
		if exclude {
			part = strings.TrimSpace(strings.TrimPrefix(part, "!"))
		}
		if part == "" {
			return PrefixList{}, errors.New("an empty CIDR (double comma) in the list")
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return PrefixList{}, fmt.Errorf("%q is not a CIDR: %w", raw, err)
		}
		if exclude {
			out.Excludes = append(out.Excludes, p)
		} else {
			out.Includes = append(out.Includes, p)
		}
	}
	return out, nil
}

// NetgroupForAddr names the netgroup whose prefix most specifically contains
// addr: the highest Bits() wins, and a tie breaks by name ascending so the
// answer is deterministic however the rows were inserted. A group that is
// registered but does not contain addr is not a match, and an address inside
// any group's exclusion matches nothing. It returns ErrNotFound when no
// registered group contains addr, and the error when the lookup fails so the
// caller can tell a miss from a broken read.
func (s *Store) NetgroupForAddr(ctx context.Context, addr netip.Addr) (string, error) {
	groups, err := s.ListNetgroups(ctx)
	if err != nil {
		return "", err
	}
	name, _, ok := ClassifyAddr(addr, groups)
	if !ok {
		return "", ErrNotFound
	}
	return name, nil
}

// ClassifyAddr returns the netgroup and the specific prefix that most
// specifically contains addr. An address inside any group's exclude matches
// nothing: an exclusion is a deliberate hole in the operator's map, so it wins
// over every include, even one that also covers the address. Otherwise the
// highest Bits() wins, and a tie breaks by name ascending. ok=false is the
// miss. It is the in-memory classifier NetgroupForAddr, ClassifyNetgroups, and
// the CLI share.
func ClassifyAddr(addr netip.Addr, groups []Netgroup) (name string, prefix netip.Prefix, ok bool) {
	addr = addr.Unmap()
	for _, g := range groups {
		for _, p := range g.Excludes {
			if p.Contains(addr) {
				return "", netip.Prefix{}, false
			}
		}
	}

	best := ""
	bestBits := -1
	for _, g := range groups {
		for _, p := range g.Prefixes {
			if !p.Contains(addr) {
				continue
			}
			if p.Bits() > bestBits || (p.Bits() == bestBits && (best == "" || g.Name < best)) {
				best, bestBits, prefix = g.Name, p.Bits(), p
			}
		}
	}
	if best == "" {
		return "", netip.Prefix{}, false
	}
	return best, prefix, true
}

// ClassifyNetgroups classifies every address in one in-memory pass, returning
// the group each one most specifically falls in keyed by the address's
// canonical (unmapped) string. An address no registered group covers — or one
// an exclusion knocks out — reads "away", the same miss label the engine
// stamps on an event. Callers that already hold the netgroups load them once
// and classify all their targets, instead of a NetgroupForAddr round-trip per
// target.
func ClassifyNetgroups(addrs []netip.Addr, groups []Netgroup) map[string]string {
	out := make(map[string]string, len(addrs))
	for _, addr := range addrs {
		addr = addr.Unmap()
		if name, _, ok := ClassifyAddr(addr, groups); ok {
			out[addr.String()] = name
		} else {
			out[addr.String()] = NetgroupAway
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
	includes, excludes, err := decodePrefixes(prefixes)
	if err != nil {
		return Netgroup{}, err
	}
	g.Prefixes = includes
	g.Excludes = excludes
	g.CreatedAt = time.UnixMilli(createdAt).UTC()
	g.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return g, nil
}

// encodePrefixes joins the includes and the `!`-marked excludes comma-separated
// for the prefixes column. The column holds plain text only because it is never
// queried structurally; the lookups parse every row, which is fine at the
// handful-of-groups scale a single operator keeps.
func encodePrefixes(includes, excludes []netip.Prefix) string {
	parts := make([]string, 0, len(includes)+len(excludes))
	for _, p := range includes {
		parts = append(parts, p.String())
	}
	for _, p := range excludes {
		parts = append(parts, "!"+p.String())
	}
	return strings.Join(parts, ",")
}

// decodePrefixes splits a prefixes column back into its includes and excludes.
// A row that reads is a row that parses: CreateNetgroup and UpdateNetgroup
// validated every prefix before storing it.
func decodePrefixes(s string) (includes, excludes []netip.Prefix, err error) {
	if s == "" {
		return nil, nil, nil
	}
	for _, part := range strings.Split(s, ",") {
		exclude := strings.HasPrefix(part, "!")
		if exclude {
			part = strings.TrimPrefix(part, "!")
		}
		p, perr := netip.ParsePrefix(part)
		if perr != nil {
			return nil, nil, fmt.Errorf("store: netgroup prefix %q does not parse: %w", part, perr)
		}
		if exclude {
			excludes = append(excludes, p)
		} else {
			includes = append(includes, p)
		}
	}
	return includes, excludes, nil
}
