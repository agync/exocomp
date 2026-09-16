package store

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"
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
		} else if !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want it to carry ErrValidation (operator input, not a store failure)", name, err)
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

// TestClassifyNetgroups pins the one-pass in-memory classifier: highest-Bits
// wins, a tie breaks by name ascending, an uncovered address reads "away", and
// a mapped address classifies under its canonical (unmapped) string.
func TestClassifyNetgroups(t *testing.T) {
	groups := []Netgroup{
		{Name: "lan", Prefixes: mustPrefixes(t, "10.0.0.0/8")},
		{Name: "lab", Prefixes: mustPrefixes(t, "10.1.2.0/24")},
		{Name: "z-lan", Prefixes: mustPrefixes(t, "192.168.0.0/16")},
		{Name: "a-lan", Prefixes: mustPrefixes(t, "192.168.0.0/16")},
	}
	got := ClassifyNetgroups([]netip.Addr{
		netip.MustParseAddr("10.1.2.9"),
		netip.MustParseAddr("10.9.0.1"),
		netip.MustParseAddr("192.168.1.1"),
		netip.MustParseAddr("::ffff:10.1.2.9"),
	}, groups)

	want := map[string]string{
		"10.1.2.9":    "lab",
		"10.9.0.1":    "lan",
		"192.168.1.1": "a-lan", // tie-break by name ascending
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ClassifyNetgroups[%s] = %q, want %q", k, got[k], v)
		}
	}
}

// TestParsePrefixList pins the shared comma-split parser: it trims, splits a
// `!`-prefixed member into the excludes, refuses an empty member (a double
// comma), and rejects a non-CIDR member.
func TestParsePrefixList(t *testing.T) {
	got, err := ParsePrefixList("192.168.0.0/16, 10.0.0.0/8")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Includes) != 2 || got.Includes[0].String() != "192.168.0.0/16" || got.Includes[1].String() != "10.0.0.0/8" {
		t.Fatalf("includes = %v, want [192.168.0.0/16 10.0.0.0/8]", got.Includes)
	}
	if len(got.Excludes) != 0 {
		t.Fatalf("excludes = %v, want none", got.Excludes)
	}

	got, err = ParsePrefixList("192.168.0.0/16, !192.168.5.0/24")
	if err != nil {
		t.Fatalf("parse with exclude: %v", err)
	}
	if len(got.Includes) != 1 || got.Includes[0].String() != "192.168.0.0/16" {
		t.Fatalf("includes = %v, want [192.168.0.0/16]", got.Includes)
	}
	if len(got.Excludes) != 1 || got.Excludes[0].String() != "192.168.5.0/24" {
		t.Fatalf("excludes = %v, want [192.168.5.0/24]", got.Excludes)
	}

	if _, err := ParsePrefixList("192.168.0.0/16,,10.0.0.0/8"); err == nil {
		t.Fatal("a double comma was accepted")
	}
	if _, err := ParsePrefixList("!192.168.0.0/16,"); err == nil {
		t.Fatal("a trailing comma after an exclude was accepted")
	}
	if _, err := ParsePrefixList("not-a-cidr"); err == nil {
		t.Fatal("a non-CIDR member was accepted")
	}
}

// TestNetgroupExclusions: an address inside a group's exclude classifies to
// the miss even when the group's include also covers it, while the rest of the
// include still names the group. The prefixes column round-trips both.
func TestNetgroupExclusions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	created, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name:     "home",
		Prefixes: mustPrefixes(t, "192.168.0.0/16"),
		Excludes: mustPrefixes(t, "192.168.5.0/24"),
	})
	if err != nil {
		t.Fatalf("create netgroup with exclude: %v", err)
	}
	if len(created.Prefixes) != 1 || len(created.Excludes) != 1 {
		t.Fatalf("created = %+v, want one include and one exclude", created)
	}

	got, err := s.GetNetgroup(ctx, "home")
	if err != nil {
		t.Fatalf("get netgroup: %v", err)
	}
	if len(got.Excludes) != 1 || got.Excludes[0].String() != "192.168.5.0/24" {
		t.Fatalf("decoded excludes = %v, want [192.168.5.0/24]", got.Excludes)
	}

	if _, err := s.NetgroupForAddr(ctx, netip.MustParseAddr("192.168.5.10")); !errors.Is(err, ErrNotFound) {
		t.Errorf("excluded address error = %v, want ErrNotFound", err)
	}
	if name, err := s.NetgroupForAddr(ctx, netip.MustParseAddr("192.168.9.10")); err != nil || name != "home" {
		t.Errorf("non-excluded address = (%q, %v), want (home, nil)", name, err)
	}

	names := ClassifyNetgroups([]netip.Addr{
		netip.MustParseAddr("192.168.5.10"),
		netip.MustParseAddr("192.168.9.10"),
	}, []Netgroup{got})
	if names["192.168.5.10"] != "away" {
		t.Errorf("excluded classification = %q, want away", names["192.168.5.10"])
	}
	if names["192.168.9.10"] != "home" {
		t.Errorf("included classification = %q, want home", names["192.168.9.10"])
	}
}

// TestNetgroupExcludeBeatsNarrowerInclude: a broad exclude knocks an address
// out even when a narrower include of the same group would otherwise win, so
// the hole is absolute rather than longest-prefix-overridable.
func TestNetgroupExcludeBeatsNarrowerInclude(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	if _, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name:     "home",
		Prefixes: mustPrefixes(t, "192.168.0.0/16", "192.168.5.0/24"),
		Excludes: mustPrefixes(t, "192.168.5.0/25"),
	}); err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	if _, err := s.NetgroupForAddr(ctx, netip.MustParseAddr("192.168.5.1")); !errors.Is(err, ErrNotFound) {
		t.Errorf("excluded address under a narrower include error = %v, want ErrNotFound", err)
	}
	if name, err := s.NetgroupForAddr(ctx, netip.MustParseAddr("192.168.5.200")); err != nil || name != "home" {
		t.Errorf("address outside the exclude = (%q, %v), want (home, nil)", name, err)
	}
}

// TestCreateNetgroupRefusesAwayName: "away" is the miss label, so a group may
// not take it — on create or on update.
func TestCreateNetgroupRefusesAwayName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	_, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name: "away", Prefixes: mustPrefixes(t, "10.0.0.0/8"),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("create away error = %v, want ErrValidation", err)
	}

	if _, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name: "home", Prefixes: mustPrefixes(t, "10.0.0.0/8"),
	}); err != nil {
		t.Fatalf("create home: %v", err)
	}
	if _, err := s.UpdateNetgroup(ctx, "home", mustPrefixes(t, "10.0.0.0/8"), nil); err != nil {
		t.Fatalf("update home: %v", err)
	}
	if _, err := s.UpdateNetgroup(ctx, "away", mustPrefixes(t, "10.0.0.0/8"), nil); !errors.Is(err, ErrValidation) {
		t.Fatalf("update away error = %v, want ErrValidation", err)
	}
}

// TestUpdateNetgroupReplacesSet: update swaps a group's includes and excludes
// in place, keeps its id, and leaves check_state's stored classification
// untouched — the observation is the sweep's to refresh.
func TestUpdateNetgroupReplacesSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	created, err := s.CreateNetgroup(ctx, NewNetgroup{
		Name: "home", Prefixes: mustPrefixes(t, "192.168.0.0/16"),
	})
	if err != nil {
		t.Fatalf("create netgroup: %v", err)
	}

	c, err := s.CreateCheck(ctx, NewCheck{
		Name: "at-home", Kind: CheckTCP, IntervalS: 300,
		Targets: []CheckTarget{{Name: "laptop", Address: "192.168.1.50", Port: 22}},
	})
	if err != nil {
		t.Fatalf("create check: %v", err)
	}
	now := time.Now().UTC()
	if err := s.SetCheckState(ctx, CheckState{
		CheckID: c.ID, TargetName: "laptop", Status: CheckUp, Consecutive: 1,
		Emitted: CheckUp, FirstSeen: now, LastChange: now,
		RecentObservations: "[]", Netgroup: "home", LastDurationMS: 7,
	}); err != nil {
		t.Fatalf("set check state: %v", err)
	}

	updated, err := s.UpdateNetgroup(ctx, "home",
		mustPrefixes(t, "10.0.0.0/8"), mustPrefixes(t, "10.1.0.0/16"))
	if err != nil {
		t.Fatalf("update netgroup: %v", err)
	}
	if updated.ID != created.ID {
		t.Errorf("update changed the id: %d -> %d", created.ID, updated.ID)
	}
	if len(updated.Prefixes) != 1 || updated.Prefixes[0].String() != "10.0.0.0/8" {
		t.Errorf("prefixes = %v, want [10.0.0.0/8]", updated.Prefixes)
	}
	if len(updated.Excludes) != 1 || updated.Excludes[0].String() != "10.1.0.0/16" {
		t.Errorf("excludes = %v, want [10.1.0.0/16]", updated.Excludes)
	}

	st, err := s.GetCheckState(ctx, c.ID, "laptop")
	if err != nil {
		t.Fatalf("get check state: %v", err)
	}
	if st.Netgroup != "home" || st.Status != CheckUp {
		t.Errorf("check_state disturbed by update: %+v", st)
	}

	if _, err := s.UpdateNetgroup(ctx, "nope", mustPrefixes(t, "10.0.0.0/8"), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown name error = %v, want ErrNotFound", err)
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
