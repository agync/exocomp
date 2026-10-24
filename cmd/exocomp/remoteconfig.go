package main

import (
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
	"exocomp.dev/exocomp/internal/wire"
)

// This file holds the remote paths of the silence, mute, check and netgroup
// verbs and of `event list`. Each sends the verb to the selected daemon, which
// validates it as a local run does, and prints what the local verb prints. The
// list verbs render the daemon's wire views through the code the local verbs
// use. A removal answers 204, so its --output json document is the client's own
// small envelope naming what was removed.

// wireTime reads an RFC3339 wire timestamp in UTC. It only feeds display, so an
// unparseable one reads as the zero time.
func wireTime(v string) time.Time {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// --- silences and mutes ----------------------------------------------------

func runRemoteSilenceAdd(cmd *cobra.Command, rc *client.Client, scope, value, reason string,
	duration time.Duration) error {
	view, err := rc.CreateSilence(cmd.Context(), client.HoldRequest{
		ScopeKind: scope, ScopeValue: value, Reason: reason, For: duration.String(),
	})
	if err != nil {
		return remoteAdminFail(cmd, "silence add", err)
	}
	return remoteAdminOut(cmd, client.KindSilence, view, func() {
		printSilenceAdded(cmd, view.ID, view.ScopeKind, view.ScopeValue, wireTime(view.ExpiresAt))
	})
}

func runRemoteSilenceRm(cmd *cobra.Command, rc *client.Client, id int64) error {
	if err := rc.RemoveSilence(cmd.Context(), id); err != nil {
		return remoteAdminFail(cmd, "silence rm", err)
	}
	return remoteAdminOut(cmd, client.KindSilenceLifted, map[string]int64{"id": id}, func() {
		printSilenceLifted(cmd, id)
	})
}

func runRemoteMuteAdd(cmd *cobra.Command, rc *client.Client, scope, value, reason string,
	duration time.Duration) error {
	view, err := rc.CreateMute(cmd.Context(), client.HoldRequest{
		ScopeKind: scope, ScopeValue: value, Reason: reason, For: duration.String(),
	})
	if err != nil {
		return remoteAdminFail(cmd, "mute add", err)
	}
	return remoteAdminOut(cmd, client.KindMute, view, func() {
		printMuteAdded(cmd, view.ID, view.ScopeKind, view.ScopeValue, wireTime(view.ExpiresAt))
	})
}

func runRemoteMuteRm(cmd *cobra.Command, rc *client.Client, id int64) error {
	if err := rc.RemoveMute(cmd.Context(), id); err != nil {
		return remoteAdminFail(cmd, "mute rm", err)
	}
	return remoteAdminOut(cmd, client.KindMuteLifted, map[string]int64{"id": id}, func() {
		printMuteLifted(cmd, id)
	})
}

// --- checks ----------------------------------------------------------------

func runRemoteCheckAdd(cmd *cobra.Command, rc *client.Client, req client.CheckRequest, interval time.Duration) error {
	view, err := rc.CreateCheck(cmd.Context(), req)
	if err != nil {
		return remoteAdminFail(cmd, "check add", err)
	}
	return remoteAdminOut(cmd, wire.KindCheck, view, func() {
		printCheckAdded(cmd, view.Name, view.Kind, len(view.Targets), interval)
	})
}

func runRemoteCheckUpdate(cmd *cobra.Command, rc *client.Client, name string, req client.CheckRequest) error {
	view, err := rc.UpdateCheck(cmd.Context(), name, req)
	if err != nil {
		return remoteAdminFail(cmd, "check update", err)
	}
	return remoteAdminOut(cmd, wire.KindCheck, view, func() {
		printCheckUpdated(cmd, view.Name, len(view.Targets), view.IntervalS)
	})
}

func runRemoteCheckSetEnabled(cmd *cobra.Command, rc *client.Client, name string, enabled bool) error {
	view, err := rc.SetCheckEnabled(cmd.Context(), name, enabled)
	if err != nil {
		return remoteAdminFail(cmd, "check "+enabledVerb(enabled), err)
	}
	return remoteAdminOut(cmd, wire.KindCheck, view, func() { printCheckEnabled(cmd, view.Name, view.Enabled) })
}

func runRemoteCheckRm(cmd *cobra.Command, rc *client.Client, name string) error {
	if err := rc.RemoveCheck(cmd.Context(), name); err != nil {
		return remoteAdminFail(cmd, "check rm", err)
	}
	return remoteAdminOut(cmd, client.KindCheckRemoved, map[string]string{"name": name}, func() {
		printCheckRemoved(cmd, name)
	})
}

// runRemoteCheckProbe probes on the daemon, so the result is what the daemon's
// network sees, not this machine's.
func runRemoteCheckProbe(cmd *cobra.Command, rc *client.Client, name, only string) error {
	view, err := rc.ProbeCheck(cmd.Context(), name, only)
	if err != nil {
		return remoteAdminFail(cmd, "check probe", err)
	}
	var printErr error
	if err := remoteAdminOut(cmd, wire.KindCheckProbe, view, func() {
		printErr = printCheckProbe(cmd, view.Targets)
	}); err != nil {
		return err
	}
	return printErr
}

// --- netgroups -------------------------------------------------------------

func runRemoteNetgroupAdd(cmd *cobra.Command, rc *client.Client, name, prefixes string) error {
	view, err := rc.CreateNetgroup(cmd.Context(), name, prefixes)
	if err != nil {
		return remoteAdminFail(cmd, "netgroup add", err)
	}
	return remoteAdminOut(cmd, wire.KindNetgroup, view, func() {
		printNetgroupAdded(cmd, view.Name, wireNetgroupLabel(view))
	})
}

func runRemoteNetgroupUpdate(cmd *cobra.Command, rc *client.Client, name, prefixes string) error {
	view, err := rc.UpdateNetgroup(cmd.Context(), name, prefixes)
	if err != nil {
		return remoteAdminFail(cmd, "netgroup update", err)
	}
	return remoteAdminOut(cmd, wire.KindNetgroup, view, func() {
		printNetgroupUpdated(cmd, view.Name, wireNetgroupLabel(view))
	})
}

func runRemoteNetgroupRm(cmd *cobra.Command, rc *client.Client, name string) error {
	if err := rc.RemoveNetgroup(cmd.Context(), name); err != nil {
		return remoteAdminFail(cmd, "netgroup rm", err)
	}
	return remoteAdminOut(cmd, client.KindNetgroupRemoved, map[string]string{"name": name}, func() {
		printNetgroupRemoved(cmd, name)
	})
}
