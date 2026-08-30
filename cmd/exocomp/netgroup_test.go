package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// runNetgroupCmd executes one `netgroup` invocation against the whole root
// command, so the persistent --output flag resolves exactly as it does for a
// real invocation.
func runNetgroupCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	err := root.Execute()
	return out.String(), err
}

func netgroups(t *testing.T) []store.Netgroup {
	t.Helper()

	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	rows, err := st.ListNetgroups(context.Background())
	if err != nil {
		t.Fatalf("list netgroups: %v", err)
	}
	return rows
}

// TestNetgroupAddListRmRoundTrip drives the verbs against a scratch data dir:
// add parses and stores the CIDRs, list renders them (text and JSON), rm
// removes the row, and a second rm reports it gone.
func TestNetgroupAddListRmRoundTrip(t *testing.T) {
	useDataDir(t, t.TempDir())

	if _, err := runNetgroupCmd(t, "netgroup", "add", "home", "192.168.0.0/16,10.0.0.0/8"); err != nil {
		t.Fatalf("netgroup add: %v", err)
	}
	rows := netgroups(t)
	if len(rows) != 1 || rows[0].Name != "home" ||
		len(rows[0].Prefixes) != 2 || rows[0].Prefixes[0].String() != "192.168.0.0/16" {
		t.Fatalf("netgroups = %+v, want one home netgroup with both prefixes", rows)
	}

	out, err := runNetgroupCmd(t, "netgroup", "list")
	if err != nil {
		t.Fatalf("netgroup list: %v", err)
	}
	if !strings.Contains(out, "home") || !strings.Contains(out, "192.168.0.0/16,10.0.0.0/8") {
		t.Errorf("list text = %q, want the group name and both prefixes", out)
	}

	out, err = runNetgroupCmd(t, "netgroup", "list", "--output", "json")
	if err != nil {
		t.Fatalf("netgroup list --output json: %v", err)
	}
	for _, want := range []string{`"kind":"netgroup.list"`, `"name":"home"`,
		`"prefixes":["192.168.0.0/16","10.0.0.0/8"]`} {
		if !strings.Contains(out, want) {
			t.Errorf("list json missing %q:\n%s", want, out)
		}
	}

	if _, err := runNetgroupCmd(t, "netgroup", "rm", "home"); err != nil {
		t.Fatalf("netgroup rm: %v", err)
	}
	if rows := netgroups(t); len(rows) != 0 {
		t.Errorf("netgroups after rm = %+v, want none", rows)
	}

	out, err = runNetgroupCmd(t, "netgroup", "rm", "home")
	if err == nil {
		t.Fatalf("second netgroup rm succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("second rm error = %v, want one naming the miss", err)
	}
}

// TestNetgroupAddRejectsBadInput keeps a row that can never stamp a webhook
// out of the database: an unparseable CIDR and an empty name are refused.
func TestNetgroupAddRejectsBadInput(t *testing.T) {
	useDataDir(t, t.TempDir())

	for _, args := range [][]string{
		{"add", "home", "not-a-cidr"},
		{"add", "home", "192.168.0.0/16,,10.0.0.0/8"},
		{"add", "", "10.0.0.0/8"},
		{"add", "bad name", "10.0.0.0/8"},
	} {
		out, err := runNetgroupCmd(t, args...)
		if err == nil {
			t.Fatalf("netgroup %v accepted, want a refusal:\n%s", args, out)
		}
	}
	if rows := netgroups(t); len(rows) != 0 {
		t.Errorf("refused netgroups were written anyway: %+v", rows)
	}

	if _, err := runNetgroupCmd(t, "netgroup", "add", "home", "192.168.0.0/16"); err != nil {
		t.Fatalf("valid add refused: %v", err)
	}
	out, err := runNetgroupCmd(t, "netgroup", "add", "home", "10.0.0.0/8")
	if err == nil {
		t.Fatalf("duplicate add accepted:\n%s", out)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate error = %v, want one naming the collision", err)
	}
}
