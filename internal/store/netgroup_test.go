package store

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
)

func mustPrefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("parse prefix %q: %v", c, err)
		}
		out = append(out, p)
	}
	return out
}

// TestNetgroupCRUDRoundTrip: create, read back (prefixes decoded), list, delete,
// and the ErrNotFound/ErrDuplicate errors every by-name CRUD reports.
func TestNetgroupCRUDRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	created, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name:     "home",
		Prefixes: mustPrefixes(t, "192.168.0.0/16", "10.0.0.0/8"),
	})
	if err != nil {
		t.Fatalf("create netgroup: %v", err)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Errorf("timestamps not stamped: %+v", created)
	}

	got, err := s.GetNetgroup(ctx, "home")
	if err != nil {
		t.Fatalf("get netgroup: %v", err)
	}
	if len(got.Prefixes) != 2 || got.Prefixes[0].String() != "192.168.0.0/16" ||
		got.Prefixes[1].String() != "10.0.0.0/8" {
		t.Errorf("prefixes = %v, want [192.168.0.0/16 10.0.0.0/8]", got.Prefixes)
	}

	rows, err := s.ListNetgroups(ctx)
	if err != nil {
		t.Fatalf("list netgroups: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "home" {
		t.Fatalf("list = %+v, want the one created netgroup", rows)
	}

	if _, err := s.GetNetgroup(ctx, "office"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown name error = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name: "home", Prefixes: mustPrefixes(t, "192.168.0.0/16"),
	}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate name error = %v, want ErrDuplicate", err)
	}

	if err := s.DeleteNetgroup(ctx, "home"); err != nil {
		t.Fatalf("delete netgroup: %v", err)
	}
	if _, err := s.GetNetgroup(ctx, "home"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete error = %v, want ErrNotFound", err)
	}
	if err := s.DeleteNetgroup(ctx, "home"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete error = %v, want ErrNotFound", err)
	}
}

// TestCreateNetgroupRefusesBadInput: a name outside the label-value character
// set, an empty prefix list, and an invalid prefix value are all refused
// before a row exists.
func TestCreateNetgroupRefusesBadInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	for name, n := range map[string]NewNetgroup{
		"empty name":     {Name: "", Prefixes: mustPrefixes(t, "10.0.0.0/8")},
		"space in name":  {Name: "home net", Prefixes: mustPrefixes(t, "10.0.0.0/8")},
		"slash in name":  {Name: "a/b", Prefixes: mustPrefixes(t, "10.0.0.0/8")},
		"no prefixes":    {Name: "home"},
		"invalid prefix": {Name: "home", Prefixes: []netip.Prefix{{}}},
	} {
		if _, err := s.CreateNetgroup(ctx, n); err == nil {
			t.Errorf("%s: accepted, want a refusal", name)
		}
	}

	rows, err := s.ListNetgroups(ctx)
	if err != nil {
		t.Fatalf("list netgroups: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("refused netgroups were written anyway: %+v", rows)
	}
}

// TestNetgroupForAddrLongestPrefixWins: the most specific covering prefix
// names the group, a tie resolves by name ascending, and an address inside no
// registered group reports false.
func TestNetgroupForAddrLongestPrefixWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	// wide first, narrow second: the lookup must not be "first registered".
	if _, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name: "lan", Prefixes: mustPrefixes(t, "10.0.0.0/8"),
	}); err != nil {
		t.Fatalf("create lan: %v", err)
	}
	if _, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name: "lab", Prefixes: mustPrefixes(t, "10.1.2.0/24"),
	}); err != nil {
		t.Fatalf("create lab: %v", err)
	}

	cases := []struct {
		name string
		addr string
		want string
		ok   bool
	}{
		{"exact /24 match wins over /8", "10.1.2.9", "lab", true},
		{"inside /8 but outside /24", "10.9.0.1", "lan", true},
		{"no registered group", "192.168.1.1", "", false},
	}
	for _, tc := range cases {
		got, err := s.NetgroupForAddr(ctx, netip.MustParseAddr(tc.addr))
		if tc.ok && err != nil {
			t.Errorf("%s: NetgroupForAddr(%s) error = %v, want the group name", tc.name, tc.addr, err)
			continue
		}
		if !tc.ok && !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: NetgroupForAddr(%s) error = %v, want ErrNotFound", tc.name, tc.addr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: NetgroupForAddr(%s) = %q, want %q", tc.name, tc.addr, got, tc.want)
		}
	}

	// An IPv4-mapped IPv6 peer (what net/http hands out) must land in the same
	// group as its IPv4 twin, not fall through as unmatched.
	if got, err := s.NetgroupForAddr(ctx, netip.MustParseAddr("::ffff:10.1.2.9")); err != nil || got != "lab" {
		t.Errorf("mapped peer = (%q, %v), want (lab, nil)", got, err)
	}
}

// TestNetgroupForAddrTieBreaksByNameAscending: two groups covering an address
// with equally specific prefixes resolve to the alphabetically first name,
// whichever group was inserted first.
func TestNetgroupForAddrTieBreaksByNameAscending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	// Insert the alphabetically-later group first, so the tie-break is
	// observable against insertion order.
	if _, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name: "z-lan", Prefixes: mustPrefixes(t, "10.0.0.0/8"),
	}); err != nil {
		t.Fatalf("create z-lan: %v", err)
	}
	if _, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name: "a-lan", Prefixes: mustPrefixes(t, "10.0.0.0/8"),
	}); err != nil {
		t.Fatalf("create a-lan: %v", err)
	}

	got, err := s.NetgroupForAddr(ctx, netip.MustParseAddr("10.1.1.1"))
	if err != nil || got != "a-lan" {
		t.Errorf("NetgroupForAddr = (%q, %v), want (a-lan, nil)", got, err)
	}
}

// TestMigration0029UpgradesFrom028 pins a database at user_version 28 and opens
// it with this binary: an operator upgrading in place must get the netgroup
// table and usable CRUD, not a migration that only runs on a fresh file.
func TestMigration0029UpgradesFrom028(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigrations(t, dbPath, 28)

	raw, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var count int
	err = raw.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'netgroup'`).Scan(&count)
	if err != nil {
		t.Fatalf("probe pre-migration schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	if count != 0 {
		t.Fatalf("netgroup table exists at user_version 28, want it introduced by 0029")
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open at v28 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 29 {
		t.Fatalf("schema version = %d after open, want >= 29", version)
	}

	if _, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name: "home", Prefixes: mustPrefixes(t, "192.168.0.0/16"),
	}); err != nil {
		t.Fatalf("create netgroup on migrated schema: %v", err)
	}
	rows, err := s.ListNetgroups(ctx)
	if err != nil {
		t.Fatalf("list netgroups: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "home" {
		t.Fatalf("list = %+v, want the one created netgroup", rows)
	}
}
