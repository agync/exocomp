package main

import (
	"strings"
	"testing"
)

func runPolicy(t *testing.T, args ...string) (string, error) {
	return runCobra(t, newPolicyCmd, args...)
}

const deleteRuleExpr = `plan.resource_changes.exists(c, c.change.actions.exists(a, a == "delete"))`

func TestPolicyAddListRm(t *testing.T) {
	initForKit(t)

	out, err := runPolicy(t, "add", "no-deletes",
		"--workspace", "homelab", "--expr", deleteRuleExpr, "--action", "deny")
	if err != nil {
		t.Fatalf("policy add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "no-deletes") || !strings.Contains(out, "deny") {
		t.Fatalf("policy add output = %q", out)
	}

	out, err = runPolicy(t, "list")
	if err != nil {
		t.Fatalf("policy list: %v (%s)", err, out)
	}
	if !strings.Contains(out, "no-deletes") || !strings.Contains(out, "homelab") {
		t.Fatalf("policy list output = %q", out)
	}

	if out, err := runPolicy(t, "rm", "no-deletes"); err != nil {
		t.Fatalf("policy rm: %v (%s)", err, out)
	}
	out, err = runPolicy(t, "list")
	if err != nil {
		t.Fatalf("policy list after rm: %v (%s)", err, out)
	}
	if !strings.Contains(out, "no plan policies") {
		t.Fatalf("policy list after rm = %q", out)
	}
}

// TestPolicyAddRejectsBadExpr proves an expression that cannot compile is
// refused at add time, so a policy that could never decide is never stored.
func TestPolicyAddRejectsBadExpr(t *testing.T) {
	initForKit(t)

	// A non-bool expression: it parses, but the engine needs a bool verdict.
	_, err := runPolicy(t, "add", "bad",
		"--workspace", "homelab", "--expr", "plan.resource_changes", "--action", "deny")
	if err == nil {
		t.Fatal("policy add with a non-bool expr: want an error, got none")
	}

	// A syntactically invalid expression.
	_, err = runPolicy(t, "add", "bad2",
		"--workspace", "homelab", "--expr", "plan.resource_changes.exists(c,", "--action", "deny")
	if err == nil {
		t.Fatal("policy add with invalid CEL: want an error, got none")
	}

	// The rejected policies must not have been written.
	out, listErr := runPolicy(t, "list")
	if listErr != nil {
		t.Fatalf("policy list: %v", listErr)
	}
	if !strings.Contains(out, "no plan policies") {
		t.Fatalf("a rejected policy was stored: %q", out)
	}
}

func TestPolicyAddRejectsUnknownAction(t *testing.T) {
	initForKit(t)
	_, err := runPolicy(t, "add", "x",
		"--workspace", "homelab", "--expr", "true", "--action", "allow")
	if err == nil {
		t.Fatal("policy add with an unknown action: want an error, got none")
	}
}
