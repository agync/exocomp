package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Artifact is a file produced by a job: a log, a plan, a rendered diff.
type Artifact struct {
	ID     int64
	JobID  int64
	Kind   string
	Path   string
	SHA256 string
	Size   int64
	// PlanSerial is the workspace serial a saved plan was made against. Zero
	// for non-plan artifacts and for plans recorded before the column existed.
	// The plan detail and the workspace's stale-refusal read it.
	PlanSerial int64
}

// CreateArtifact records an artifact against a job.
func (s *Store) CreateArtifact(ctx context.Context, a Artifact) (Artifact, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO artifact (job_id, kind, path, sha256, size, plan_serial) VALUES (?, ?, ?, ?, ?, ?)`,
		a.JobID, a.Kind, a.Path, a.SHA256, a.Size, a.PlanSerial)
	if err != nil {
		return Artifact{}, wrap("create artifact", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Artifact{}, wrap("create artifact", err)
	}
	a.ID = id
	return a, nil
}

// GetArtifact returns the artifact with the given id.
func (s *Store) GetArtifact(ctx context.Context, id int64) (Artifact, error) {
	var a Artifact
	err := s.db.QueryRowContext(ctx, `
		SELECT id, job_id, kind, path, sha256, size, plan_serial FROM artifact WHERE id = ?`, id).
		Scan(&a.ID, &a.JobID, &a.Kind, &a.Path, &a.SHA256, &a.Size, &a.PlanSerial)
	if err != nil {
		return Artifact{}, wrap("get artifact", err)
	}
	return a, nil
}

// scanArtifact reads one artifact row into an Artifact. The column order is
// the one every artifact query uses: id, job_id, kind, path, sha256, size,
// plan_serial.
func scanArtifact(rows *sql.Rows) (Artifact, error) {
	var a Artifact
	err := rows.Scan(&a.ID, &a.JobID, &a.Kind, &a.Path, &a.SHA256, &a.Size, &a.PlanSerial)
	return a, err
}

// ListArtifacts returns a job's artifacts in creation order.
func (s *Store) ListArtifacts(ctx context.Context, jobID int64) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, kind, path, sha256, size, plan_serial
		FROM artifact WHERE job_id = ? ORDER BY id`, jobID)
	if err != nil {
		return nil, wrap("list artifacts", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list artifacts", err)
	}
	return out, nil
}

// ListPlanArtifacts returns the newest `limit` plan artifacts of the jobs
// whose runbook matches (e.g. a workspace's `workspace:<name>` plans), newest
// artifact first. kind is the artifact kind to keep (the caller passes the
// reactive plan kind — the store cannot import reactive). Zero result, not an
// error, when nothing matches.
func (s *Store) ListPlanArtifacts(ctx context.Context, runbook, kind string, limit int) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.job_id, a.kind, a.path, a.sha256, a.size, a.plan_serial
		FROM artifact a JOIN job j ON j.id = a.job_id
		WHERE j.runbook = ? AND a.kind = ?
		ORDER BY a.id DESC LIMIT ?`, runbook, kind, limit)
	if err != nil {
		return nil, wrap("list plan artifacts", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list plan artifacts", err)
	}
	return out, nil
}

// DeleteArtifact removes an artifact row. The file itself is the caller's
// problem.
func (s *Store) DeleteArtifact(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM artifact WHERE id = ?`, id)
	if err != nil {
		return wrap("delete artifact", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("delete artifact", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: delete artifact %d: %w", id, ErrNotFound)
	}
	return nil
}

// ReRecorded reports whether the sealed plan's sha256 is already linked to a
// job other than the one that sealed it. An apply records its consumed plan by
// re-inserting the same sha256 under the applying job's id; a second apply of
// the same plan must not pile up another row, so the CLI consults this before
// inserting. sealJob is the sealing plan job's id (0 for an operator's own
// plan file, which was never sealed by a job).
func (s *Store) ReRecorded(ctx context.Context, sha256 string, sealJob int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM artifact WHERE sha256 = ? AND job_id != ?`, sha256, sealJob).Scan(&n)
	if err != nil {
		return false, wrap("re-recorded artifact", err)
	}
	return n > 0, nil
}
