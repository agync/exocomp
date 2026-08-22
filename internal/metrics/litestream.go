package metrics

import (
	"context"
	"errors"
	"time"

	"exocomp.dev/exocomp/internal/litestream"
	"exocomp.dev/exocomp/internal/store"
)

const (
	helpLSConfigured = "1 when a Litestream replica target is configured, else 0."
	helpLSFrameTS    = "Unix time of the newest replicated WAL frame observed on the replica."
	helpLSStale      = "Seconds since the newest replicated WAL frame (+Inf when unknown: no replica, a remote replica exocomp cannot read, or no frames yet). This is the RPO age."
)

// litestreamGauges reports Litestream replication freshness — the RPO age. When
// no replica is configured the staleness is +Inf and configured is 0, so an
// operator reads "unknown" honestly rather than a zero that would look fresh.
// For a local (file) replica exocomp reads the newest WAL frame's age directly;
// a remote replica's frame age can only be read by litestream itself, so it too
// is reported as +Inf (unknown) rather than guessed.
func (r *Registry) litestreamGauges(ctx context.Context, now time.Time) ([]gauge, error) {
	raw, err := r.store.GetSetting(ctx, litestream.SettingReplicaTarget)
	if errors.Is(err, store.ErrNotFound) || raw == "" {
		return []gauge{
			{name: "exocomp_litestream_replica_configured", help: helpLSConfigured, value: 0},
			{name: "exocomp_litestream_replication_staleness_seconds", help: helpLSStale, value: inf()},
		}, nil
	}
	if err != nil {
		return nil, err
	}

	out := []gauge{{name: "exocomp_litestream_replica_configured", help: helpLSConfigured, value: 1}}

	target, perr := litestream.ParseTarget(raw)
	if perr != nil {
		// A stored target that no longer parses is still "configured but its
		// freshness is unknown" — not a scrape failure.
		out = append(out, gauge{name: "exocomp_litestream_replication_staleness_seconds", help: helpLSStale, value: inf()})
		return out, nil
	}

	_, mtime, ok, ferr := litestream.ReplicaFreshness(target, now)
	if ferr != nil {
		return nil, ferr
	}
	if !ok {
		out = append(out, gauge{name: "exocomp_litestream_replication_staleness_seconds", help: helpLSStale, value: inf()})
		return out, nil
	}
	out = append(out,
		gauge{name: "exocomp_litestream_replication_last_frame_timestamp_seconds", help: helpLSFrameTS, value: unix(mtime)},
		gauge{name: "exocomp_litestream_replication_staleness_seconds", help: helpLSStale, value: age(now, mtime)},
	)
	return out, nil
}
