package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Runner build modes (byo-runner design §2). cached builds the image once and
// reuses it until the recipe changes; onthefly rebuilds every run against the
// layer cache.
const (
	RunnerModeCached   = "cached"
	RunnerModeOnTheFly = "onthefly"
)

// runnerHashKind is the §7.1 kind/version prefix mixed into the content hash.
// Bump it when the recipe format changes so an image built under the old
// semantics never collides onto the new ones.
const runnerHashKind = "runner-v1"

// Runner is a registered execution-environment recipe. It is inert: the
// container tier that would build and run the image is deferred, so nothing in
// this binary executes a runner. Registration exists so a definition is stored,
// hashed, and backed up with the data volume ahead of that tier.
type Runner struct {
	ID   int64
	Name string
	// Mode is RunnerModeCached or RunnerModeOnTheFly.
	Mode string
	Base string
	// Bootstrap is the Model A script body; Containerfile is the Model B path.
	// They are mutually exclusive.
	Bootstrap     string
	Containerfile string
	// Packages is stored sorted and deduplicated (canonical form).
	Packages    []string
	AnsibleCore string
	// MemoryMax and CPUQuota are the caps the deferred tier would enforce,
	// in systemd syntax ("2G", "200%"). Stored verbatim, parsed by nothing yet.
	MemoryMax string
	CPUQuota  string
	// ContentHash is the design §7.1 runner-hash. It is derived, not supplied:
	// every write recomputes it from the definition columns.
	ContentHash string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// BootstrapSHA256 is the digest of the bootstrap body, the form the content
// hash and the wire projections carry instead of the body itself.
func (r Runner) BootstrapSHA256() string {
	if r.Bootstrap == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(r.Bootstrap))
	return hex.EncodeToString(sum[:])
}

// BootstrapLines counts the lines of the bootstrap body, so a caller can show
// its size without carrying it.
func (r Runner) BootstrapLines() int {
	if r.Bootstrap == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(r.Bootstrap, "\n"), "\n") + 1
}

// runnerHashInput is the canonical JSON the content hash is taken over. Field
// order is the marshalled order, so the bytes are stable across writes.
//
// Mode is deliberately absent: design §2 requires that switching between the
// two build models never forces a rebuild of an identical recipe. The caps are
// absent for the same reason — they bound a run, they do not change the image.
type runnerHashInput struct {
	Base            string   `json:"base"`
	BootstrapSHA256 string   `json:"bootstrap_sha256"`
	Containerfile   string   `json:"containerfile"`
	Packages        []string `json:"packages"`
	AnsibleCore     string   `json:"ansible_core"`
}

// RunnerHash is the design §7.1 content hash: the kind/version prefix, a NUL,
// then the canonical JSON of the definition, sha256'd and truncated to 32 hex
// characters. It ports deps.hashManifest's construction, including its
// fail-safe direction: hashing a field that turns out not to matter only
// over-resolves, missing one that does under-resolves.
func RunnerHash(r Runner) string {
	body, err := json.Marshal(runnerHashInput{
		Base:            r.Base,
		BootstrapSHA256: r.BootstrapSHA256(),
		Containerfile:   r.Containerfile,
		Packages:        CanonicalPackages(r.Packages),
		AnsibleCore:     r.AnsibleCore,
	})
	if err != nil {
		// Every field is a string or a []string; marshalling cannot fail.
		panic("store: marshal runner hash input: " + err.Error())
	}
	h := sha256.New()
	h.Write([]byte(runnerHashKind))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// CanonicalPackages sorts and deduplicates a package list, dropping blanks. The
// canonical form is what is stored and what is hashed, so the order an operator
// typed never changes a runner's identity. The result is non-nil so it
// marshals as [] rather than null.
func CanonicalPackages(pkgs []string) []string {
	out := make([]string, 0, len(pkgs))
	seen := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

const runnerColumns = `id, name, mode, base, bootstrap, containerfile,
	packages_json, ansible_core, memory_max, cpu_quota, content_hash,
	created_at, updated_at`

// CreateRunner registers a runner definition. Packages are canonicalized and
// the content hash is recomputed here, never taken from the caller. A repeated
// name returns ErrDuplicate.
func (s *Store) CreateRunner(ctx context.Context, r Runner) (Runner, error) {
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	r.Packages = CanonicalPackages(r.Packages)
	r.ContentHash = RunnerHash(r)

	packages, err := encodePackages(r.Packages)
	if err != nil {
		return Runner{}, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO runner (name, mode, base, bootstrap, containerfile,
		                    packages_json, ansible_core, memory_max, cpu_quota,
		                    content_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, r.Mode, r.Base, r.Bootstrap, r.Containerfile, packages,
		r.AnsibleCore, r.MemoryMax, r.CPUQuota, r.ContentHash,
		r.CreatedAt.UnixMilli(), r.UpdatedAt.UnixMilli())
	if err != nil {
		return Runner{}, wrap("create runner", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Runner{}, wrap("create runner", err)
	}
	return s.GetRunner(ctx, id)
}

// GetRunner returns the runner with the given id.
func (s *Store) GetRunner(ctx context.Context, id int64) (Runner, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runnerColumns+` FROM runner WHERE id = ?`, id)
	r, err := scanRunner(row)
	if err != nil {
		return Runner{}, wrap("get runner", err)
	}
	return r, nil
}

// RunnerByName returns the runner with the given name, or ErrNotFound.
func (s *Store) RunnerByName(ctx context.Context, name string) (Runner, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runnerColumns+` FROM runner WHERE name = ?`, name)
	r, err := scanRunner(row)
	if err != nil {
		return Runner{}, wrap("runner by name", err)
	}
	return r, nil
}

// ListRunners returns every registered runner by name.
func (s *Store) ListRunners(ctx context.Context) ([]Runner, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runnerColumns+` FROM runner ORDER BY name`)
	if err != nil {
		return nil, wrap("list runners", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Runner
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, wrap("list runners", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list runners", err)
	}
	return out, nil
}

// UpdateRunner replaces the definition of the runner named r.Name, recomputing
// the content hash and stamping updated_at. It is the import path's
// create-or-update half; an unknown name returns ErrNotFound.
func (s *Store) UpdateRunner(ctx context.Context, r Runner) (Runner, error) {
	r.Packages = CanonicalPackages(r.Packages)
	r.ContentHash = RunnerHash(r)
	r.UpdatedAt = time.Now().UTC()

	packages, err := encodePackages(r.Packages)
	if err != nil {
		return Runner{}, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE runner SET mode = ?, base = ?, bootstrap = ?, containerfile = ?,
		                  packages_json = ?, ansible_core = ?, memory_max = ?,
		                  cpu_quota = ?, content_hash = ?, updated_at = ?
		WHERE name = ?`,
		r.Mode, r.Base, r.Bootstrap, r.Containerfile, packages, r.AnsibleCore,
		r.MemoryMax, r.CPUQuota, r.ContentHash, r.UpdatedAt.UnixMilli(), r.Name)
	if err != nil {
		return Runner{}, wrap("update runner", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Runner{}, wrap("update runner", err)
	}
	if affected == 0 {
		return Runner{}, fmt.Errorf("store: update runner %q: %w", r.Name, ErrNotFound)
	}
	return s.RunnerByName(ctx, r.Name)
}

// DeleteRunner removes a runner definition by name.
func (s *Store) DeleteRunner(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM runner WHERE name = ?`, name)
	if err != nil {
		return wrap("delete runner", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete runner", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete runner %q: %w", name, ErrNotFound)
	}
	return nil
}

func scanRunner(row rowScanner) (Runner, error) {
	var (
		r                    Runner
		packages             string
		createdAt, updatedAt int64
	)
	if err := row.Scan(&r.ID, &r.Name, &r.Mode, &r.Base, &r.Bootstrap,
		&r.Containerfile, &packages, &r.AnsibleCore, &r.MemoryMax, &r.CPUQuota,
		&r.ContentHash, &createdAt, &updatedAt); err != nil {
		return Runner{}, err
	}
	decoded, err := decodePackages(packages)
	if err != nil {
		return Runner{}, err
	}
	r.Packages = decoded
	r.CreatedAt = time.UnixMilli(createdAt).UTC()
	r.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return r, nil
}

func encodePackages(pkgs []string) (string, error) {
	if len(pkgs) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(pkgs)
	if err != nil {
		return "", fmt.Errorf("store: encode runner packages: %w", err)
	}
	return string(b), nil
}

func decodePackages(s string) ([]string, error) {
	if s == "" || s == "[]" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("store: decode runner packages: %w", err)
	}
	return out, nil
}
