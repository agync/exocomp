package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/deps"
)

// lastInvocation is the most recent argv the fake ansible-galaxy recorded.
func lastInvocation(t *testing.T, counter string) string {
	t.Helper()
	body, err := os.ReadFile(counter)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read invocations: %v", err)
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	return lines[len(lines)-1]
}

func TestRunVendorInstallsCollectionsIntoTheCache(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real ansible-galaxy")
	}
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
	code, err := runVendor(context.Background(), &out, &out, requirements, "")
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
	root := deps.FindProjectRoot(filepath.Dir(requirements))
	reqBody, err := os.ReadFile(requirements)
	if err != nil {
		t.Fatalf("read requirements: %v", err)
	}
	installed := filepath.Join(dir, collectionsCacheDir, deps.SourceKey(root), galaxyRequirementsHash(reqBody), "ansible_collections", "community", "sops")
	if info, statErr := os.Stat(installed); statErr != nil || !info.IsDir() {
		t.Fatalf("collection not installed at %s: %v", installed, statErr)
	}
}

func TestRunVendorFailsWithTheJobOnABadRequirementsFile(t *testing.T) {
	if testing.Short() {
		t.Skip("requires real ansible-galaxy")
	}
	if _, err := exec.LookPath("ansible-galaxy"); err != nil {
		t.Skip("ansible-galaxy not on PATH")
	}
	useDataDir(t, t.TempDir())

	var out bytes.Buffer
	code, err := runVendor(context.Background(), &out, &out, filepath.Join(t.TempDir(), "missing.yml"), "")
	if err == nil {
		t.Fatalf("vendor exit code = %d for a missing requirements file:\n%s", code, out.String())
	}
}

// TestRunVendorInstallsIntoPerSourceDir proves `playbook vendor` is source-keyed
// (ADR 0015): the install target derives from the --requirements file's project
// root and content hash, not the shared cache dir. A fake ansible-galaxy records
// the -p it was handed, so no network is involved.
func TestRunVendorInstallsIntoPerSourceDir(t *testing.T) {
	useDataDir(t, t.TempDir())

	binDir := t.TempDir()
	invocations := filepath.Join(t.TempDir(), "galaxy-invocations")
	fakeGalaxyBinary(t, binDir, invocations)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	project := t.TempDir()
	requirements := filepath.Join(project, "requirements.yml")
	writeFile(t, requirements, "collections:\n  - community.sops\n")

	var out bytes.Buffer
	code, err := runVendor(context.Background(), &out, &out, requirements, "")
	if err != nil {
		t.Fatalf("vendor: %v", err)
	}
	if code != 0 {
		t.Fatalf("vendor exit code = %d:\n%s", code, out.String())
	}

	dir, err := dataDir()
	if err != nil {
		t.Fatalf("data dir: %v", err)
	}
	root := deps.FindProjectRoot(filepath.Dir(requirements))
	body, err := os.ReadFile(requirements)
	if err != nil {
		t.Fatalf("read requirements: %v", err)
	}
	want := filepath.Join(dir, collectionsCacheDir, deps.SourceKey(root), galaxyRequirementsHash(body))
	if got := lastInvocation(t, invocations); !strings.Contains(got, "-p "+want) {
		t.Fatalf("install target not per-source (want %s): %s", want, got)
	}
	if _, err := os.Stat(filepath.Join(dir, collectionsCacheDir, "ansible_collections")); !os.IsNotExist(err) {
		t.Errorf("vendor wrote collections into the shared cache dir")
	}
}
