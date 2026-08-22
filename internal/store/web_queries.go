package store

import "context"

// ListWorkflowIDs returns distinct workflow ids ordered by most-recent step
// activity first, capped at limit (a non-positive limit lifts the cap). It
// backs the web surface's workflows list, which shows in-flight and recently
// finished chains together; ListIncompleteWorkflows covers only the ones with a
// step still to run, so it cannot surface a workflow that already reached its
// end. This is a read-only projection of the same workflow_step ledger.
func (s *Store) ListWorkflowIDs(ctx context.Context, limit int) ([]string, error) {
	query := `
		SELECT workflow_id, MAX(updated_at) AS last
		FROM workflow_step
		GROUP BY workflow_id
		ORDER BY last DESC, workflow_id DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list workflow ids", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var (
			id   string
			last int64
		)
		if err := rows.Scan(&id, &last); err != nil {
			return nil, wrap("list workflow ids", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list workflow ids", err)
	}
	return out, nil
}
