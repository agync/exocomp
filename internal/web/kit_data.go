package web

import (
	"context"
	"strconv"

	"exocomp.dev/exocomp/internal/store"
)

// kitMeta reads the Recovery-kit staleness state (SPEC §7 A3): the date it
// was printed and how many invalidating changes landed since. Stale tints the
// identity strip's meta when a change landed after the last print.
func (s *Server) kitMeta(ctx context.Context) kitMeta {
	m := kitMeta{}
	if v, err := s.store.GetSetting(ctx, store.SettingKitPrintedAt); err == nil {
		m.Printed = v
	}
	if v, err := s.store.GetSetting(ctx, store.SettingKitChanges); err == nil {
		if n, perr := strconv.Atoi(v); perr == nil {
			m.Changes = n
		}
	}
	m.Stale = m.Changes > 0
	return m
}
