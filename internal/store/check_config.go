package store

import (
	"context"
	"fmt"
)

// SetCheckAggregate updates a check's aggregate rollup config in place. It is
// the mutation `check update --aggregate` needs: UpdateCheck replaces targets
// and interval only, so the opt-in aggregate flag and its threshold have their
// own setter, validated against the check's current target count exactly as
// CreateCheck validates at add time.
func (s *Store) SetCheckAggregate(ctx context.Context, name string, aggregate bool, minUp int) error {
	cur, err := s.GetCheck(ctx, name)
	if err != nil {
		return err
	}
	if minUp < 0 || minUp > len(cur.Targets) {
		return fmt.Errorf("%w: store: check %q min_up must be between 0 and %d (0 = every target), got %d",
			ErrValidation, name, len(cur.Targets), minUp)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE check_row SET aggregate = ?, min_up = ? WHERE name = ?`, aggregate, minUp, name)
	if err != nil {
		return wrap("set check aggregate", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return wrap("set check aggregate", err)
	}
	if affected == 0 {
		return fmt.Errorf("store: set check aggregate %q: %w", name, ErrNotFound)
	}
	return nil
}
