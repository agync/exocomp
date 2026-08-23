package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Egress postures a registered CLI tool may declare. They are the whole
// network policy a registration can express, because Landlock filters outbound
// TCP by port and not by destination.
const (
	// CLIEgressNone is the default: the run reaches no network at all.
	CLIEgressNone = "none"
	// CLIEgressHTTPS adds port 443 to that run's sandbox profile.
	CLIEgressHTTPS = "https"
)

// cliToolNameMax bounds a name so it stays a usable job-row label.
const cliToolNameMax = 64

// CLITool is one entry in the allow-list of binaries a governed CLI run may
// execute (v1.1 roadmap R1). A tool that is not registered is refused before a
// job row exists, so the allow-list — not the caller — decides what can run.
type CLITool struct {
	ID   int64
	Name string
	// Binary is the executable: a bare name resolved on PATH at run time, or an
	// absolute path pinning the exact file. It is argv[0] of a direct exec and
	// is never handed to a shell.
	Binary string
	// FixedArgs are prepended to the operator's own arguments, so a
	// registration can pin the part of an invocation that must not vary.
	FixedArgs []string
	// ArgPattern is an anchored regexp every operator-supplied argument must
	// match. Empty leaves only the structural guard.
	ArgPattern string
	// Egress is CLIEgressNone or CLIEgressHTTPS.
	Egress      string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ArgMatcher compiles the tool's argument pattern, anchored at both ends so a
// pattern narrows the whole argument rather than matching somewhere inside it.
// A tool with no pattern returns a nil matcher, which admits any argument the
// structural guard already accepted.
func (t CLITool) ArgMatcher() (*regexp.Regexp, error) {
	if t.ArgPattern == "" {
		return nil, nil
	}
	re, err := regexp.Compile(`\A(?:` + t.ArgPattern + `)\z`)
	if err != nil {
		return nil, fmt.Errorf("store: cli tool %q arg pattern: %w", t.Name, err)
	}
	return re, nil
}

// ValidateCLITool enforces the rules the schema cannot: the name shape, a
// non-empty binary that is a single token, a compilable argument pattern, and a
// known egress posture.
func ValidateCLITool(t CLITool) error {
	if t.Name == "" || len(t.Name) > cliToolNameMax {
		return fmt.Errorf("cli tool name %q must be 1-%d characters", t.Name, cliToolNameMax)
	}
	for i, r := range t.Name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_' || r == '.') && i > 0:
		default:
			return fmt.Errorf("cli tool name %q must start with a lowercase letter or digit "+
				"and hold only [a-z0-9._-]", t.Name)
		}
	}
	if strings.TrimSpace(t.Binary) == "" {
		return fmt.Errorf("cli tool %q needs a binary", t.Name)
	}
	// The binary becomes argv[0] of an exec, so a value carrying whitespace or a
	// control character is a command line someone expected a shell to split.
	if strings.ContainsFunc(t.Binary, func(r rune) bool { return r <= ' ' }) {
		return fmt.Errorf("cli tool %q binary %q must be one token: this is exec'd directly, "+
			"never split by a shell", t.Name, t.Binary)
	}
	// A fixed argument carrying a control character would fail every run of the
	// tool, so the registration is where it is refused rather than each run.
	for _, a := range t.FixedArgs {
		if strings.ContainsFunc(a, unicode.IsControl) {
			return fmt.Errorf("cli tool %q fixed argument %q contains a control character", t.Name, a)
		}
	}
	if t.Egress != CLIEgressNone && t.Egress != CLIEgressHTTPS {
		return fmt.Errorf("cli tool %q egress must be %s or %s, not %q",
			t.Name, CLIEgressNone, CLIEgressHTTPS, t.Egress)
	}
	if _, err := t.ArgMatcher(); err != nil {
		return err
	}
	return nil
}

const cliToolColumns = `id, name, binary, fixed_args_json, arg_pattern, egress,
	description, created_at, updated_at`

// CreateCLITool registers one allow-list entry. A repeated name returns
// ErrDuplicate.
func (s *Store) CreateCLITool(ctx context.Context, t CLITool) (CLITool, error) {
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = t.CreatedAt
	}
	if t.Egress == "" {
		t.Egress = CLIEgressNone
	}

	args, err := encodeCLIArgs(t.FixedArgs)
	if err != nil {
		return CLITool{}, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO cli_tool (name, binary, fixed_args_json, arg_pattern, egress,
		                      description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.Binary, args, t.ArgPattern, t.Egress, t.Description,
		t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	if err != nil {
		return CLITool{}, wrap("create cli tool", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return CLITool{}, wrap("create cli tool", err)
	}
	return s.getCLITool(ctx, id)
}

func (s *Store) getCLITool(ctx context.Context, id int64) (CLITool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cliToolColumns+` FROM cli_tool WHERE id = ?`, id)
	t, err := scanCLITool(row)
	if err != nil {
		return CLITool{}, wrap("get cli tool", err)
	}
	return t, nil
}

// CLIToolByName returns the allow-list entry with the given name, or
// ErrNotFound. It is the lookup that decides whether a run may be enqueued at
// all.
func (s *Store) CLIToolByName(ctx context.Context, name string) (CLITool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cliToolColumns+` FROM cli_tool WHERE name = ?`, name)
	t, err := scanCLITool(row)
	if err != nil {
		return CLITool{}, wrap("cli tool by name", err)
	}
	return t, nil
}

// ListCLITools returns every registered tool by name.
func (s *Store) ListCLITools(ctx context.Context) ([]CLITool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cliToolColumns+` FROM cli_tool ORDER BY name`)
	if err != nil {
		return nil, wrap("list cli tools", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CLITool
	for rows.Next() {
		t, err := scanCLITool(rows)
		if err != nil {
			return nil, wrap("list cli tools", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list cli tools", err)
	}
	return out, nil
}

// DeleteCLITool removes an allow-list entry by name.
func (s *Store) DeleteCLITool(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cli_tool WHERE name = ?`, name)
	if err != nil {
		return wrap("delete cli tool", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete cli tool", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete cli tool %q: %w", name, ErrNotFound)
	}
	return nil
}

func scanCLITool(row rowScanner) (CLITool, error) {
	var (
		t                    CLITool
		args                 string
		createdAt, updatedAt int64
	)
	if err := row.Scan(&t.ID, &t.Name, &t.Binary, &args, &t.ArgPattern, &t.Egress,
		&t.Description, &createdAt, &updatedAt); err != nil {
		return CLITool{}, err
	}
	decoded, err := decodeCLIArgs(args)
	if err != nil {
		return CLITool{}, err
	}
	t.FixedArgs = decoded
	t.CreatedAt = time.UnixMilli(createdAt).UTC()
	t.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return t, nil
}

func encodeCLIArgs(args []string) (string, error) {
	if len(args) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("store: encode cli tool args: %w", err)
	}
	return string(b), nil
}

func decodeCLIArgs(s string) ([]string, error) {
	if s == "" || s == "[]" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("store: decode cli tool args: %w", err)
	}
	return out, nil
}
