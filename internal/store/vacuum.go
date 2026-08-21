package store

import (
	"context"
	"errors"
)

// VacuumInto writes a consistent snapshot of the database to path. It is one
// statement: SQLite folds the WAL in and writes a plain, defragmented database
// file, so the snapshot is readable by any sqlite3 build with no exocomp code
// on the path.
//
// VACUUM INTO refuses an existing destination, and that refusal is kept: a
// half-written snapshot must never be mistaken for a good one.
func (s *Store) VacuumInto(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("store: vacuum into: empty destination path")
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return wrap("vacuum into "+path, err)
	}
	return nil
}
