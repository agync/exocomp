package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorDoesNotCreateTheDataDir: doctor reports, it does not act. Creating
// the dir made every run report on doctor's own handiwork, and created it 0755
// where `exocomp init` uses 0700.
func TestDoctorDoesNotCreateTheDataDir(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	dir := filepath.Join(root, "exocomp")

	var out bytes.Buffer
	if err := runDoctor(&out); err != nil {
		t.Fatalf("doctor on an absent data dir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("doctor created %s (stat err = %v)", dir, err)
	}
	if !strings.Contains(out.String(), "absent") {
		t.Errorf("doctor did not report the dir as absent: %q", out.String())
	}
	if !strings.Contains(out.String(), "creatable") {
		t.Errorf("doctor did not say the absent dir can be created: %q", out.String())
	}
}

func TestDoctorReportsAnExistingPrivateDataDir(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	dir := filepath.Join(root, "exocomp")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var out bytes.Buffer
	if err := runDoctor(&out); err != nil {
		t.Fatalf("doctor on a 0700 data dir: %v", err)
	}
	if !strings.Contains(out.String(), "mode 0700 — writable") {
		t.Errorf("doctor did not report the mode: %q", out.String())
	}
}

// TestDoctorRejectsALooseDataDirMode: the dir holds host.key, so a group- or
// world-readable mode is a finding, not a detail to pass over.
func TestDoctorRejectsALooseDataDirMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o750, 0o701} {
		root := t.TempDir()
		useDataDir(t, root)
		dir := filepath.Join(root, "exocomp")
		if err := os.Mkdir(dir, mode); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatalf("chmod: %v", err)
		}

		var out bytes.Buffer
		err := runDoctor(&out)
		if err == nil {
			t.Errorf("doctor accepted a %04o data dir; output = %q", mode, out.String())
		}
		if !strings.Contains(out.String(), "TOO PERMISSIVE") {
			t.Errorf("doctor did not name the mode %04o as the problem: %q", mode, out.String())
		}
	}
}

// TestInitCreatesAPrivateDataDir is the other half: doctor now insists on
// 0700, so the command that creates the dir has to produce it.
func TestInitCreatesAPrivateDataDir(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	dir := filepath.Join(root, "exocomp")

	var out bytes.Buffer
	cmd := newInitCmd()
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("init created no data dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("init created %s as %04o, want 0700", dir, got)
	}

	var doctorOut bytes.Buffer
	if err := runDoctor(&doctorOut); err != nil {
		t.Fatalf("doctor rejects the dir init just made: %v\n%s", err, doctorOut.String())
	}
}

// TestDoctorProbesUvAndGit: uv and git became runtime dependencies with ADR
// 0011 (project-dependency resolution and git sources), so doctor must list
// them beside the other optional tools rather than staying silent about a gap
// an operator only discovers mid-run.
func TestDoctorProbesUvAndGit(t *testing.T) {
	root := t.TempDir()
	useDataDir(t, root)
	if err := os.Mkdir(filepath.Join(root, "exocomp"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var out bytes.Buffer
	if err := runDoctor(&out); err != nil {
		t.Fatalf("doctor: %v\n%s", err, out.String())
	}
	for _, tool := range []string{"uv", "git"} {
		if !strings.Contains(out.String(), tool) {
			t.Errorf("doctor did not probe %q:\n%s", tool, out.String())
		}
	}
}
