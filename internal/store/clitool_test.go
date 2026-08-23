package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func cliToolFixture() CLITool {
	return CLITool{
		Name:        "kubectl",
		Binary:      "/usr/bin/kubectl",
		FixedArgs:   []string{"--context", "homelab"},
		ArgPattern:  `[a-z0-9./=:-]+`,
		Egress:      CLIEgressHTTPS,
		Description: "cluster reads and rollouts",
	}
}

// TestMigration0019UpgradesFrom18 pins a database at user_version 18 and opens
// it with this binary: an operator upgrading in place must get the allow-list
// table and a usable CRUD, not a migration that only runs on a fresh file.
func TestMigration0019UpgradesFrom18(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "exocomp.db")
	pinAtMigrations(t, dbPath, 18)

	raw, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	var count int
	err = raw.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'cli_tool'`).Scan(&count)
	if err != nil {
		t.Fatalf("probe pre-migration schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	if count != 0 {
		t.Fatalf("cli_tool table exists at user_version 18, want it introduced by 0019")
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open at v18 for upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version < 19 {
		t.Fatalf("schema version = %d after open, want >= 19", version)
	}

	if _, err := s.CreateCLITool(ctx, cliToolFixture()); err != nil {
		t.Fatalf("create cli tool on migrated schema: %v", err)
	}
	rows, err := s.ListCLITools(ctx)
	if err != nil {
		t.Fatalf("list cli tools: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "kubectl" {
		t.Fatalf("list = %+v, want the one created tool", rows)
	}
}

func TestCLIToolCRUDRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	created, err := s.CreateCLITool(ctx, cliToolFixture())
	if err != nil {
		t.Fatalf("create cli tool: %v", err)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Errorf("timestamps not stamped: %+v", created)
	}

	got, err := s.CLIToolByName(ctx, "kubectl")
	if err != nil {
		t.Fatalf("cli tool by name: %v", err)
	}
	if got.Binary != "/usr/bin/kubectl" || got.Egress != CLIEgressHTTPS {
		t.Errorf("round trip = %+v", got)
	}
	if len(got.FixedArgs) != 2 || got.FixedArgs[0] != "--context" || got.FixedArgs[1] != "homelab" {
		t.Errorf("fixed args = %v, want [--context homelab]", got.FixedArgs)
	}

	// The lookup that gates enqueue must report an unregistered name as
	// ErrNotFound, not as an empty row.
	if _, err := s.CLIToolByName(ctx, "curl"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unregistered tool error = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateCLITool(ctx, cliToolFixture()); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate name error = %v, want ErrDuplicate", err)
	}

	if err := s.DeleteCLITool(ctx, "kubectl"); err != nil {
		t.Fatalf("delete cli tool: %v", err)
	}
	if _, err := s.CLIToolByName(ctx, "kubectl"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete error = %v, want ErrNotFound", err)
	}
	if err := s.DeleteCLITool(ctx, "kubectl"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete error = %v, want ErrNotFound", err)
	}
}

// TestCLIToolArgMatcherIsAnchored: an unanchored pattern would admit an
// argument that merely *contains* an allowed substring, which is the whole
// value of registering a pattern at all.
func TestCLIToolArgMatcherIsAnchored(t *testing.T) {
	t.Parallel()

	tool := CLITool{Name: "kubectl", Binary: "kubectl", ArgPattern: `[a-z]+`}
	re, err := tool.ArgMatcher()
	if err != nil {
		t.Fatalf("arg matcher: %v", err)
	}
	if !re.MatchString("rollout") {
		t.Error("an argument matching the whole pattern was refused")
	}
	if re.MatchString("rollout; rm -rf /") {
		t.Error("pattern matched a substring: it is not anchored")
	}
	// A newline must not let a second line satisfy the pattern; \A and \z are
	// what \n-tolerant ^ and $ would not give.
	if re.MatchString("rollout\nrm") {
		t.Error("pattern matched across a newline")
	}

	none := CLITool{Name: "t", Binary: "t"}
	if re, err := none.ArgMatcher(); err != nil || re != nil {
		t.Errorf("no pattern should yield a nil matcher, got %v %v", re, err)
	}
}

func TestValidateCLIToolRefusesUnsafeRegistrations(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		tool CLITool
	}{
		{"empty name", CLITool{Binary: "kubectl", Egress: CLIEgressNone}},
		{"upper name", CLITool{Name: "Kubectl", Binary: "kubectl", Egress: CLIEgressNone}},
		{"no binary", CLITool{Name: "kubectl", Egress: CLIEgressNone}},
		// A registration written as a command line is the mistake this kind
		// exists to prevent: nothing here is ever split by a shell.
		{"shell string binary", CLITool{Name: "kubectl", Binary: "kubectl get pods", Egress: CLIEgressNone}},
		{"unknown egress", CLITool{Name: "kubectl", Binary: "kubectl", Egress: "any"}},
		{"bad pattern", CLITool{Name: "kubectl", Binary: "kubectl", Egress: CLIEgressNone, ArgPattern: "("}},
	} {
		if err := ValidateCLITool(tc.tool); err == nil {
			t.Errorf("%s: registration accepted, want a refusal", tc.name)
		}
	}

	if err := ValidateCLITool(cliToolFixture()); err != nil {
		t.Errorf("valid registration refused: %v", err)
	}
}
