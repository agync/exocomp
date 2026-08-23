package executor

import (
	"slices"
	"testing"
)

// TestTofuEnvInjectsWorkspaceVar proves the exocomp workspace name reaches a
// config as var.exocomp_workspace, the stand-in for terraform.workspace under
// the HTTP backend (named-state multi-env).
func TestTofuEnvInjectsWorkspaceVar(t *testing.T) {
	env, err := (&Tofu{opts: TofuOptions{Workspace: "prod"}}).env(t.TempDir(), "", nil)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if !slices.Contains(env, "TF_VAR_exocomp_workspace=prod") {
		t.Fatalf("env missing TF_VAR_exocomp_workspace=prod:\n%v", env)
	}

	// No workspace name -> no injected var (nothing to name).
	env2, err := (&Tofu{opts: TofuOptions{}}).env(t.TempDir(), "", nil)
	if err != nil {
		t.Fatalf("env (no workspace): %v", err)
	}
	if slices.Contains(env2, "TF_VAR_exocomp_workspace=") {
		t.Fatalf("injected an empty workspace var:\n%v", env2)
	}
}
