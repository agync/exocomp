package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreRequiresFrom(t *testing.T) {
	cmd := newRestoreCmd()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --from is missing")
	}
	if !strings.Contains(err.Error(), "--from is required") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestRestoreDefaultsDest(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("EXOCOMP_DATA_DIR", filepath.Join(tmp, "exocomp"))

	cmd := newRestoreCmd()
	// Provide --from with a non-existent local target so it fails on target access
	// instead of missing --dest.
	cmd.SetArgs([]string{"--from", "restic_local:" + filepath.Join(tmp, "nonexistent")})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error with nonexistent target")
	}
	// The error should be about the target or unwrap, NOT "--dest required" or missing flag.
	if strings.Contains(err.Error(), "--dest required") || strings.Contains(err.Error(), "required flag") {
		t.Fatalf("unexpected --dest required error: %v", err)
	}
}
