// Package checkprobe is the one on-demand probe runner the CLI (`check probe`),
// the MCP `probe_check` tool, and the API's probe route share. It builds each
// target's probe.Spec through store.SpecForTarget and runs it with the sweep's
// options, so what a caller sees is what the next sweep will record. It writes
// nothing (D3): no check_state row moves and no event is emitted.
package checkprobe

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"exocomp.dev/exocomp/internal/probe"
	"exocomp.dev/exocomp/internal/store"
)

// Timeout matches the sweep's bound (reactive.probeTimeout): an on-demand probe
// answers with the same deadline the sweep uses.
const Timeout = 5 * time.Second

// ErrNoTarget reports that a narrowing request named a target the check does
// not have. Callers render it in their own vocabulary (a CLI error, an
// invalid_argument envelope); the wrapped target name is the leaf message.
var ErrNoTarget = errors.New("no such target")

// Target is one probed target's outcome, before the caller projects it onto its
// own wire shape. Netgroup is the sweep's stored classification when the target
// has been observed, else a live classification of the spec's address; a kind
// with no address literal carries an empty string.
type Target struct {
	Name       string
	Up         bool
	DurationMS int64
	Netgroup   string
	Detail     string
}

// Run probes the check's targets in stored order, or just the one named by
// only, with the sweep's options (AllowLoopback=false, Timeout). A probe that
// cannot even start — a target whose spec does not validate — is an error, not
// a down, mirroring the sweep. It reads the stored netgroups and state but
// writes none.
func Run(ctx context.Context, st *store.Store, c store.Check, only string) ([]Target, error) {
	targets := c.Targets
	if only != "" {
		targets = nil
		for _, t := range c.Targets {
			if t.Name == only {
				targets = append(targets, t)
			}
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("%w: %s", ErrNoTarget, only)
		}
	}

	states, err := st.ListCheckStates(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	stored := make(map[string]string, len(states))
	for _, s := range states {
		stored[s.TargetName] = s.Netgroup
	}
	groups, err := st.ListNetgroups(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		spec, err := store.SpecForTarget(c.Kind, t)
		if err != nil {
			return nil, fmt.Errorf("check %s target %q: %w", c.Name, t.Name, err)
		}
		if err := spec.Validate(); err != nil {
			return nil, fmt.Errorf("check %s target %q: %w", c.Name, t.Name, err)
		}
		res := probe.Run(ctx, spec, probe.Options{Timeout: Timeout, AllowLoopback: false})
		out = append(out, Target{
			Name:       t.Name,
			Up:         res.Up,
			DurationMS: res.Duration.Milliseconds(),
			Netgroup:   probeNetgroup(spec, stored[t.Name], groups),
			Detail:     res.Detail,
		})
	}
	return out, nil
}

// probeNetgroup names where a target was probed: the sweep's stored
// classification when it has observed the target, else a live classification of
// the address the spec carries. A kind with no address literal (dns, metric, and
// a tls hostname) carries none.
func probeNetgroup(spec probe.Spec, stored string, groups []store.Netgroup) string {
	if stored != "" {
		return stored
	}
	addr, ok := spec.Location()
	if !ok {
		return ""
	}
	if ng, ok := store.ClassifyNetgroups([]netip.Addr{addr}, groups)[addr.Unmap().String()]; ok {
		return ng
	}
	return ""
}
