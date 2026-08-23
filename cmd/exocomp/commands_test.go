package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// builtin cobra commands are generated at Execute time and carry no Example;
// they are the only commands exempt from the guard.
func isBuiltin(c *cobra.Command) bool {
	for p := c; p != nil; p = p.Parent() {
		if p.Name() == "help" || p.Name() == "completion" {
			return true
		}
	}
	return false
}

// TestEveryRunnableCommandHasExample is the CI guard against Example rot: a new
// runnable command with no Example (authored or generated) fails here.
func TestEveryRunnableCommandHasExample(t *testing.T) {
	root := newRootCmd()

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() && !isBuiltin(c) && strings.TrimSpace(c.Example) == "" {
			t.Errorf("command %q has no Example", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// TestCommandsDumpParses proves the hidden self-describing dump is valid JSON a
// harness can consume in one call.
func TestCommandsDumpParses(t *testing.T) {
	out, err := run(t, "commands", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		SchemaVersion int `json:"schema_version"`
		Kind          string
		Data          struct {
			Path     string
			Commands []struct {
				Path     string
				Runnable bool
				Example  string
				Commands []json.RawMessage
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("commands dump is not valid JSON: %v\n%s", err, out)
	}
	if env.SchemaVersion != 1 || env.Kind != "commands" {
		t.Fatalf("unexpected envelope: version=%d kind=%q", env.SchemaVersion, env.Kind)
	}
	if env.Data.Path != "exocomp" || len(env.Data.Commands) == 0 {
		t.Fatalf("dump has no command tree: %s", out)
	}
}

// TestGeneratedExampleShape checks the fallback keeps Use's argument
// placeholders, so a generated Example stays a real invocation.
func TestGeneratedExampleShape(t *testing.T) {
	root := newRootCmd()
	got := map[string]string{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		got[c.CommandPath()] = c.Example
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)

	if ex := got["exocomp workspace apply"]; !strings.Contains(ex, "workspace apply <name>") {
		t.Errorf("workspace apply example = %q, want it to keep the <name> placeholder", ex)
	}
	if ex := got["exocomp workspace list"]; ex == "" {
		t.Errorf("workspace list must have an authored example")
	}
}
