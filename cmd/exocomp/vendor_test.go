package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunVendorInstallsCollectionsIntoTheCache(t *testing.T) {
	if _, err := exec.LookPath("ansible-galaxy"); err != nil {
		t.Skip("ansible-galaxy not on PATH")
	}
	useDataDir(t, t.TempDir())

	requirements := filepath.Join(t.TempDir(), "requirements.yml")
	body := "collections:\n  - name: community.sops\n"
	if err := os.WriteFile(requirements, []byte(body), 0o600); err != nil {
		t.Fatalf("write requirements: %v", err)
	}

	var out bytes.Buffer
	code, err := runVendor(context.Background(), &out, &out, requirements)
	if err != nil {
		t.Fatalf("vendor: %v", err)
	}
	if code != 0 {
		t.Fatalf("vendor exit code = %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "state: succeeded") {
		t.Errorf("output missing a succeeded job:\n%s", out.String())
	}

	dir, err := dataDir()
	if err != nil {
		t.Fatalf("data dir: %v", err)
	}
	installed := filepath.Join(dir, collectionsCacheDir, "ansible_collections", "community", "sops")
	if info, statErr := os.Stat(installed); statErr != nil || !info.IsDir() {
		t.Fatalf("collection not installed at %s: %v", installed, statErr)
	}
}

func TestRunVendorFailsWithTheJobOnABadRequirementsFile(t *testing.T) {
	if _, err := exec.LookPath("ansible-galaxy"); err != nil {
		t.Skip("ansible-galaxy not on PATH")
	}
	useDataDir(t, t.TempDir())

	var out bytes.Buffer
	code, err := runVendor(context.Background(), &out, &out, filepath.Join(t.TempDir(), "missing.yml"))
	if err != nil {
		t.Fatalf("vendor: %v", err)
	}
	if code == 0 {
		t.Fatalf("vendor exit code = 0 for a missing requirements file:\n%s", out.String())
	}
}
