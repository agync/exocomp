package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// runSteps runs each command through runRoot and checks its text.
func runSteps(t *testing.T, steps []struct {
	args []string
	want string
}) {
	t.Helper()
	for _, step := range steps {
		out, _, err := runRoot(t, step.args)
		if err != nil {
			t.Fatalf("%v: %v\n%s", step.args, err, out)
		}
		if !strings.Contains(out, step.want) {
			t.Fatalf("%v printed %q, want %q", step.args, out, step.want)
		}
	}
}

// Silences and mutes land in the daemon's store, list from it, and lift there.
func TestSilenceAndMuteVerbsChangeTheDaemonStore(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")
	ctx := context.Background()

	runSteps(t, []struct {
		args []string
		want string
	}{
		{[]string{"silence", "add", "--host", "web1", "--for", "2h", "--reason", "patching"}, "silence 1: host web1 until "},
		{[]string{"silence", "list"}, "patching"},
		{[]string{"mute", "add", "--alert", "DiskFull", "--for", "1h"}, "mute 1: alert DiskFull until "},
		{[]string{"mute", "list"}, "alert DiskFull"},
	})
	if s, err := d.st.ListSilences(ctx, true); err != nil || len(s) != 1 || s[0].CreatedVia != "api" {
		t.Fatalf("daemon silences = %+v (%v)", s, err)
	}
	if m, err := d.st.ListNotifyMutes(ctx, true); err != nil || len(m) != 1 {
		t.Fatalf("daemon mutes = %+v (%v)", m, err)
	}

	runSteps(t, []struct {
		args []string
		want string
	}{
		{[]string{"silence", "rm", "1"}, "silence 1 lifted"},
		{[]string{"silence", "list"}, "no active silences"},
		{[]string{"silence", "list", "--all"}, "no silences"},
		{[]string{"mute", "rm", "1"}, "mute 1 lifted"},
	})
	if s, _ := d.st.ListSilences(ctx, true); len(s) != 0 {
		t.Fatal("silence rm left the daemon's silence")
	}
	if m, _ := d.st.ListNotifyMutes(ctx, true); len(m) != 0 {
		t.Fatal("mute rm left the daemon's mute")
	}
	if _, _, err := runRoot(t, []string{"silence", "rm", "9"}); err == nil || !strings.Contains(err.Error(), "silence rm") {
		t.Fatalf("lifting an unknown silence: %v", err)
	}
	assertNoLocalStore(t)
}

// Checks and netgroups are added, re-pointed, switched, probed and removed on the
// daemon, which also lists them.
func TestCheckAndNetgroupVerbsChangeTheDaemonStore(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")
	ctx := context.Background()

	runSteps(t, []struct {
		args []string
		want string
	}{
		{[]string{"netgroup", "add", "home", "192.168.0.0/16,!192.168.5.0/24"}, "netgroup home added: 192.168.0.0/16,!192.168.5.0/24"},
		{[]string{"netgroup", "update", "home", "10.0.0.0/8"}, "netgroup home updated: 10.0.0.0/8"},
		{[]string{"netgroup", "list"}, "10.0.0.0/8"},
		{[]string{"check", "add", "lan", "--kind", "tcp", "--interval", "5m", "--target", "a=127.0.0.1:9"},
			"check lan (tcp) added: 1 target(s) every 5m0s"},
		{[]string{"check", "update", "lan", "--interval", "10m", "--target", "a=127.0.0.1:9", "--target", "b=127.0.0.1:10"},
			"check lan updated: 2 target(s) every 10m"},
		{[]string{"check", "disable", "lan"}, "check lan disabled"},
		{[]string{"check", "enable", "lan"}, "check lan enabled"},
	})
	groups, err := d.st.ListNetgroups(ctx)
	if err != nil || len(groups) != 1 || groups[0].Prefixes[0].String() != "10.0.0.0/8" {
		t.Fatalf("daemon netgroups = %+v (%v)", groups, err)
	}
	checks, err := d.st.ListChecks(ctx, false)
	if err != nil || len(checks) != 1 || len(checks[0].Targets) != 2 || checks[0].IntervalS != 600 || !checks[0].Enabled {
		t.Fatalf("daemon checks = %+v (%v)", checks, err)
	}

	// The probe runs on the daemon; a target that is not up reads as DOWN.
	out, _, err := runRoot(t, []string{"check", "probe", "lan", "--target", "a"})
	if err != nil || !strings.Contains(out, "TARGET") || !strings.Contains(out, "a  ") {
		t.Fatalf("check probe printed %q (%v)", out, err)
	}
	if _, _, err := runRoot(t, []string{"check", "probe", "lan", "--target", "nope"}); err == nil ||
		!strings.Contains(err.Error(), "nope") {
		t.Fatalf("probe of an unknown target: %v", err)
	}
	// A kind field that does not belong to the stored kind is refused by the daemon.
	if _, _, err := runRoot(t, []string{"check", "update", "lan", "--target", "a=127.0.0.1:9", "--min-days", "3"}); err == nil ||
		!strings.Contains(err.Error(), "min_days") {
		t.Fatalf("update with a foreign kind field: %v", err)
	}

	runSteps(t, []struct {
		args []string
		want string
	}{
		{[]string{"check", "rm", "lan"}, "check lan removed"},
		{[]string{"netgroup", "rm", "home"}, "netgroup home removed"},
	})
	if c, _ := d.st.ListChecks(ctx, false); len(c) != 0 {
		t.Fatal("check rm left the daemon's check")
	}
	if g, _ := d.st.ListNetgroups(ctx); len(g) != 0 {
		t.Fatal("netgroup rm left the daemon's netgroup")
	}
	assertNoLocalStore(t)
}

// event list reads the daemon's events with the same filters and table.
func TestEventListReadsTheDaemonStore(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")
	ctx := context.Background()

	for _, e := range []store.Event{
		{Source: "alertmanager", Outcome: store.OutcomeFired, MatchedRule: "disk", Reason: "matched disk"},
		{Source: "check", Outcome: store.OutcomeIgnored, Reason: "no rule"},
	} {
		if _, err := d.st.CreateEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	runSteps(t, []struct {
		args []string
		want string
	}{
		{[]string{"event", "list"}, "matched disk"},
		{[]string{"event", "list", "--outcome", "ignored"}, "no rule"},
		{[]string{"event", "list", "--source", "alertmanager", "--limit", "0"}, "disk"},
	})
	out, _, err := runRoot(t, []string{"event", "list", "--outcome", "ignored"})
	if err != nil || strings.Contains(out, "matched disk") {
		t.Fatalf("outcome filter leaked: %q (%v)", out, err)
	}
	assertNoLocalStore(t)
}

// A readonly token is refused by the writes, naming the operator scope, and the
// daemon's store is unchanged.
func TestRemoteConfigVerbsNeedAnOperatorToken(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeReadonly, true)
	t.Setenv(remoteEnv, "")
	ctx := context.Background()

	for _, args := range [][]string{
		{"silence", "add", "--global", "--yes-really", "--for", "1h"},
		{"mute", "add", "--host", "h", "--for", "1h"},
		{"check", "add", "c", "--kind", "tcp", "--interval", "5m", "--target", "a=127.0.0.1:9"},
		{"netgroup", "add", "home", "10.0.0.0/8"},
	} {
		_, _, err := runRoot(t, args)
		if err == nil || !strings.Contains(err.Error(), "operator") {
			t.Fatalf("%v with a readonly token: want a refusal naming the operator scope, got %v", args, err)
		}
	}
	s, _ := d.st.ListSilences(ctx, false)
	m, _ := d.st.ListNotifyMutes(ctx, false)
	c, _ := d.st.ListChecks(ctx, false)
	g, _ := d.st.ListNetgroups(ctx)
	if len(s)+len(m)+len(c)+len(g) != 0 {
		t.Fatalf("a refused verb changed the daemon: %d silences, %d mutes, %d checks, %d netgroups",
			len(s), len(m), len(c), len(g))
	}
	assertNoLocalStore(t)
}

// Under --output json the daemon's envelope is re-emitted, and a removal, which
// the daemon answers without a body, gets the client's own small envelope.
func TestRemoteConfigVerbsAsJSON(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")

	type envelope struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}
	run := func(args ...string) envelope {
		t.Helper()
		out, _, err := runRoot(t, append([]string{"--output", "json"}, args...))
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		var env envelope
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("%v: envelope = %s (%v)", args, out, err)
		}
		return env
	}
	if env := run("netgroup", "add", "home", "10.0.0.0/8"); env.Kind != "netgroup.get" || env.Data["name"] != "home" {
		t.Fatalf("netgroup add envelope = %+v", env)
	}
	if env := run("silence", "add", "--alert", "A", "--for", "1h"); env.Kind != "silence.get" || env.Data["scope_value"] != "A" {
		t.Fatalf("silence add envelope = %+v", env)
	}
	if env := run("netgroup", "rm", "home"); env.Kind != "netgroup.removed" || env.Data["name"] != "home" {
		t.Fatalf("netgroup rm envelope = %+v", env)
	}
	if g, _ := d.st.ListNetgroups(context.Background()); len(g) != 0 {
		t.Fatal("netgroup rm left the daemon's netgroup")
	}
	assertNoLocalStore(t)
}
