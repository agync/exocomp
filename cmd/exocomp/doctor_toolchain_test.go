package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorTextMarksPinDeviations: a tool that resolves to a version other
// than its MANIFEST.json pin is marked on its own line, and the active
// manifest path is printed once — the text half of Decision 0006's diff. A fake
// restic on PATH makes the resolved version deterministic without the host's
// toolchain.
func TestDoctorTextMarksPinDeviations(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	if err := os.MkdirAll(filepath.Join(root, "exocomp"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	bin := t.TempDir()
	restic := filepath.Join(bin, "restic")
	if err := os.WriteFile(restic, []byte("#!/bin/sh\necho 'restic 9.9.9 compiled with go'\n"), 0o755); err != nil {
		t.Fatalf("write fake restic: %v", err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	manifest := filepath.Join(t.TempDir(), "MANIFEST.json")
	if err := os.WriteFile(manifest, []byte(`{"restic":"0.19.0"}`), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	t.Setenv("EXOCOMP_TOOLCHAIN_MANIFEST", manifest)

	var out bytes.Buffer
	if err := runDoctor(&out); err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, manifest) {
		t.Errorf("doctor did not print the active manifest path %q:\n%s", manifest, got)
	}
	if !strings.Contains(got, "9.9.9") || !strings.Contains(got, "— pinned 0.19.0") {
		t.Errorf("doctor did not mark restic's pin deviation:\n%s", got)
	}
}
