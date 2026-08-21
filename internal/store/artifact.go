package store

import (
	"context"
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
}

// CreateArtifact records an artifact against a job.
func (s *Store) CreateArtifact(ctx context.Context, a Artifact) (Artifact, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO artifact (job_id, kind, path, sha256, size) VALUES (?, ?, ?, ?, ?)`,
		a.JobID, a.Kind, a.Path, a.SHA256, a.Size)
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
		SELECT id, job_id, kind, path, sha256, size FROM artifact WHERE id = ?`, id).
		Scan(&a.ID, &a.JobID, &a.Kind, &a.Path, &a.SHA256, &a.Size)
	if err != nil {
		return Artifact{}, wrap("get artifact", err)
	}
	return a, nil
}

// ListArtifacts returns a job's artifacts in creation order.
func (s *Store) ListArtifacts(ctx context.Context, jobID int64) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, kind, path, sha256, size
		FROM artifact WHERE job_id = ? ORDER BY id`, jobID)
	if err != nil {
		return nil, wrap("list artifacts", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.JobID, &a.Kind, &a.Path, &a.SHA256, &a.Size); err != nil {
			return nil, fmt.Errorf("store: scan artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list artifacts", err)
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
