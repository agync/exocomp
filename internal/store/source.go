package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Source kinds (Decision 0010). git is the managed, auto-synced, deploy-keyed
// locality; path is a tree already on the box (absolute, or relative to the
// data volume). Read/write is not the kind's business — it is the Writable bit.
const (
	SourceGit  = "git"
	SourcePath = "path"
)

// Inventory kinds. A static inventory is a file inside a source; a dynamic one
// is a plugin config whose auth env is bound in EnvBinding.
const (
	InventoryStatic  = "static"
	InventoryDynamic = "dynamic"
)

// Source is where a run's tree comes from. The credential planes (vault, fleet
// key, provider env) are identical across kinds; only how the tree arrives and
// stays fresh differs.
type Source struct {
	ID              int64
	Name            string
	Kind            string
	URI             string
	Ref             string
	DeployKeySecret string
	LastSyncSHA     string
	Signed          bool
	// AllowedSigners is the SSH allowed-signers content a signed source verifies
	// its pinned ref against — exocomp-owned trust, materialized to a 0700 file at
	// sync. Empty falls back to git's own configuration.
	AllowedSigners string
	// Writable opts a path source into a read+write working directory. It
	// defaults false: a run reads the tree but the sandbox denies writes to it.
	// Ignored for a git source, whose managed checkout is always writable.
	Writable  bool
	Enabled   bool
	CreatedAt time.Time
}

// Inventory is what an ansible run targets. For a dynamic inventory EnvBinding
// maps a plugin env name to the sealed-secret name it resolves from.
type Inventory struct {
	ID         int64
	Name       string
	SourceID   *int64
	Path       string
	Kind       string
	EnvBinding map[string]string
	CreatedAt  time.Time
}

// RunSecret is one (env name -> sealed-secret name) pair under a binding name.
// It carries no secret material: the value is resolved from the envelope at
// execution time.
type RunSecret struct {
	ID         int64
	Binding    string
	EnvName    string
	SecretName string
	CreatedAt  time.Time
}

const sourceColumns = `id, name, kind, uri, ref, deploy_key_secret,
	last_sync_sha, signed, allowed_signers, writable, enabled, created_at`

// CreateSource inserts a source. A repeated name returns ErrDuplicate.
func (s *Store) CreateSource(ctx context.Context, src Source) (Source, error) {
	if src.CreatedAt.IsZero() {
		src.CreatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO source (name, kind, uri, ref, deploy_key_secret,
		                    last_sync_sha, signed, allowed_signers, writable, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		src.Name, src.Kind, src.URI, src.Ref, src.DeployKeySecret,
		src.LastSyncSHA, boolInt(src.Signed), src.AllowedSigners, boolInt(src.Writable),
		boolInt(src.Enabled), src.CreatedAt.UnixMilli())
	if err != nil {
		return Source{}, wrap("create source", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Source{}, wrap("create source", err)
	}
	return s.GetSource(ctx, id)
}

// GetSource returns the source with the given id.
func (s *Store) GetSource(ctx context.Context, id int64) (Source, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM source WHERE id = ?`, id)
	src, err := scanSource(row)
	if err != nil {
		return Source{}, wrap("get source", err)
	}
	return src, nil
}

// SourceByName returns the source with the given name, or ErrNotFound.
func (s *Store) SourceByName(ctx context.Context, name string) (Source, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM source WHERE name = ?`, name)
	src, err := scanSource(row)
	if err != nil {
		return Source{}, wrap("source by name", err)
	}
	return src, nil
}

// ListSources returns every source by name.
func (s *Store) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM source ORDER BY name`)
	if err != nil {
		return nil, wrap("list sources", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, wrap("list sources", err)
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list sources", err)
	}
	return out, nil
}

// RecordSourceSync advances a source's pinned ref and records the commit a sync
// landed on. It is how the control plane moves the trust anchor forward only
// after a gated run advances it.
func (s *Store) RecordSourceSync(ctx context.Context, id int64, ref, sha string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE source SET ref = ?, last_sync_sha = ? WHERE id = ?`, ref, sha, id)
	if err != nil {
		return wrap("record source sync", err)
	}
	return affectedOne(res, "record source sync", id)
}

// SetSourceEnabled toggles whether a source may be used by runs.
func (s *Store) SetSourceEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE source SET enabled = ? WHERE id = ?`, boolInt(enabled), id)
	if err != nil {
		return wrap("set source enabled", err)
	}
	return affectedOne(res, "set source enabled", id)
}

// DeleteSource removes a source by name. Its inventories cascade.
func (s *Store) DeleteSource(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM source WHERE name = ?`, name)
	if err != nil {
		return wrap("delete source", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete source", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete source %q: %w", name, ErrNotFound)
	}
	return nil
}

const inventoryColumns = `id, name, source_id, path, kind, env_binding_json, created_at`

// CreateInventory inserts an inventory. A repeated name returns ErrDuplicate.
func (s *Store) CreateInventory(ctx context.Context, inv Inventory) (Inventory, error) {
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = time.Now().UTC()
	}
	binding, err := encodeBinding(inv.EnvBinding)
	if err != nil {
		return Inventory{}, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO inventory (name, source_id, path, kind, env_binding_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		inv.Name, nullInt64(inv.SourceID), inv.Path, inv.Kind, binding,
		inv.CreatedAt.UnixMilli())
	if err != nil {
		return Inventory{}, wrap("create inventory", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Inventory{}, wrap("create inventory", err)
	}
	inv.ID = id
	return inv, nil
}

// InventoryByName returns the inventory with the given name, or ErrNotFound.
func (s *Store) InventoryByName(ctx context.Context, name string) (Inventory, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+inventoryColumns+` FROM inventory WHERE name = ?`, name)
	inv, err := scanInventory(row)
	if err != nil {
		return Inventory{}, wrap("inventory by name", err)
	}
	return inv, nil
}

// ListInventories returns every inventory by name.
func (s *Store) ListInventories(ctx context.Context) ([]Inventory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+inventoryColumns+` FROM inventory ORDER BY name`)
	if err != nil {
		return nil, wrap("list inventories", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Inventory
	for rows.Next() {
		inv, err := scanInventory(rows)
		if err != nil {
			return nil, wrap("list inventories", err)
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list inventories", err)
	}
	return out, nil
}

// DeleteInventory removes an inventory by name.
func (s *Store) DeleteInventory(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM inventory WHERE name = ?`, name)
	if err != nil {
		return wrap("delete inventory", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete inventory", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete inventory %q: %w", name, ErrNotFound)
	}
	return nil
}

// AddRunSecret binds one env name to a sealed-secret name under a binding name.
// A repeated (binding, env name) returns ErrDuplicate.
func (s *Store) AddRunSecret(ctx context.Context, rs RunSecret) (RunSecret, error) {
	if rs.CreatedAt.IsZero() {
		rs.CreatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO run_secret (binding, env_name, secret_name, created_at)
		VALUES (?, ?, ?, ?)`,
		rs.Binding, rs.EnvName, rs.SecretName, rs.CreatedAt.UnixMilli())
	if err != nil {
		return RunSecret{}, wrap("add run secret", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return RunSecret{}, wrap("add run secret", err)
	}
	rs.ID = id
	return rs, nil
}

// RunSecretsByBinding returns every (env name -> secret name) pair under a
// binding, ordered by env name. An empty result is not an error: the caller
// decides whether an unknown binding matters.
func (s *Store) RunSecretsByBinding(ctx context.Context, binding string) ([]RunSecret, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, binding, env_name, secret_name, created_at
		FROM run_secret WHERE binding = ? ORDER BY env_name`, binding)
	if err != nil {
		return nil, wrap("run secrets by binding", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunSecret
	for rows.Next() {
		rs, err := scanRunSecret(rows)
		if err != nil {
			return nil, wrap("run secrets by binding", err)
		}
		out = append(out, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("run secrets by binding", err)
	}
	return out, nil
}

// ListRunSecretBindings returns the distinct binding names, ordered.
func (s *Store) ListRunSecretBindings(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT binding FROM run_secret ORDER BY binding`)
	if err != nil {
		return nil, wrap("list run secret bindings", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, wrap("list run secret bindings", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list run secret bindings", err)
	}
	return out, nil
}

// DeleteRunSecretBinding removes every pair under a binding name.
func (s *Store) DeleteRunSecretBinding(ctx context.Context, binding string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM run_secret WHERE binding = ?`, binding)
	if err != nil {
		return wrap("delete run secret binding", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete run secret binding", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete run secret binding %q: %w", binding, ErrNotFound)
	}
	return nil
}

func scanSource(row rowScanner) (Source, error) {
	var (
		src                       Source
		signed, writable, enabled int64
		createdAt                 int64
	)
	if err := row.Scan(&src.ID, &src.Name, &src.Kind, &src.URI, &src.Ref,
		&src.DeployKeySecret, &src.LastSyncSHA, &signed, &src.AllowedSigners,
		&writable, &enabled, &createdAt); err != nil {
		return Source{}, err
	}
	src.Signed = signed != 0
	src.Writable = writable != 0
	src.Enabled = enabled != 0
	src.CreatedAt = time.UnixMilli(createdAt).UTC()
	return src, nil
}

func scanInventory(row rowScanner) (Inventory, error) {
	var (
		inv       Inventory
		sourceID  sql.NullInt64
		binding   string
		createdAt int64
	)
	if err := row.Scan(&inv.ID, &inv.Name, &sourceID, &inv.Path, &inv.Kind,
		&binding, &createdAt); err != nil {
		return Inventory{}, err
	}
	inv.SourceID = int64Ptr(sourceID)
	decoded, err := decodeBinding(binding)
	if err != nil {
		return Inventory{}, err
	}
	inv.EnvBinding = decoded
	inv.CreatedAt = time.UnixMilli(createdAt).UTC()
	return inv, nil
}

func scanRunSecret(row rowScanner) (RunSecret, error) {
	var (
		rs        RunSecret
		createdAt int64
	)
	if err := row.Scan(&rs.ID, &rs.Binding, &rs.EnvName, &rs.SecretName, &createdAt); err != nil {
		return RunSecret{}, err
	}
	rs.CreatedAt = time.UnixMilli(createdAt).UTC()
	return rs, nil
}

func encodeBinding(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("store: encode env binding: %w", err)
	}
	return string(b), nil
}

func decodeBinding(s string) (map[string]string, error) {
	if s == "" || s == "{}" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("store: decode env binding: %w", err)
	}
	return m, nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func affectedOne(res sql.Result, op string, id int64) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap(op, err)
	}
	if affected == 0 {
		return fmt.Errorf("store: %s %d: %w", op, id, ErrNotFound)
	}
	return nil
}
