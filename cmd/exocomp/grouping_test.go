package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestCommandGroupsRenderAndAreValid proves the help groups render and that
// every assigned GroupID is registered — an unregistered GroupID makes cobra
// panic at execute, so this is the guard when a new command joins the map.
func TestCommandGroupsRenderAndAreValid(t *testing.T) {
	root := newRootCmd()

	// Every command's GroupID (when set) must name a registered group.
	valid := map[string]bool{}
	for _, g := range root.Groups() {
		valid[g.ID] = true
	}
	for _, c := range root.Commands() {
		if c.GroupID != "" && !valid[c.GroupID] {
			t.Errorf("command %q has unregistered GroupID %q", c.Name(), c.GroupID)
		}
	}

	// The help output carries the section titles and groups a known verb.
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("help execute: %v", err)
	}
	help := out.String()
	for _, title := range []string{"Run & schedule:", "Secrets & access:", "Backup & recovery:"} {
		if !strings.Contains(help, title) {
			t.Errorf("help output missing group title %q", title)
		}
	}
}
