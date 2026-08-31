package reactive

import "context"

// sweepStep runs one item of a sweep under the shared error policy: a
// per-item failure is logged and the sweep continues to its siblings, so one
// broken check or job cannot starve the rest; a cancelled context stops the
// sweep instead of churning. It returns the item's yield and whether to keep
// going.
//
// A persistently-bad item logs once per sweep interval (each sweep re-runs
// it), the accepted cost of keeping the rest of the sweep moving.
func (e *Engine) sweepStep(ctx context.Context, what, item string, fn func() (int, error)) (int, bool) {
	if err := ctx.Err(); err != nil {
		return 0, false
	}
	n, err := fn()
	if err != nil {
		// A per-item failure at shutdown is the shutdown itself — the sweep is
		// stopping anyway, so the log would be noise. Suppress it then.
		if ctx.Err() == nil {
			e.log.Error("reactive: "+what, "item", item, "err", err)
		}
		return n, ctx.Err() == nil
	}
	return n, true
}
