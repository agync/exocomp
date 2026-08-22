package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestBackupHasNoCanaryCommand: `secrets add` seals a value from stdin for any
// kind, so a second hidden command that sealed one under a fixed kind was a
// duplicate surface over the same key material.
func TestBackupHasNoCanaryCommand(t *testing.T) {
	for _, c := range newBackupCmd().Commands() {
		if c.Name() == "canary" {
			t.Error("backup canary is back; secrets add supersedes it")
		}
	}
}

// TestAddTargetWarnsOnCredentialsInURI: a uri with userinfo is accepted, since
// refusing it would strand an operator whose remote only speaks tokens, but it
// is never accepted quietly.
func TestAddTargetWarnsOnCredentialsInURI(t *testing.T) {
	useDataDir(t, t.TempDir())

	const token = "ghp-s3cr3t-token-1f4a"
	var out, errOut bytes.Buffer
	cmd := newBackupCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"add-target", "--name", "offsite", "--kind", "git",
		"--uri", "https://bot:" + token + "@git.invalid/backup.git"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("add-target: %v", err)
	}

	if !strings.Contains(errOut.String(), "WARNING") {
		t.Errorf("add-target accepted an embedded credential silently; stderr = %q", errOut.String())
	}
	if strings.Contains(errOut.String(), token) {
		t.Errorf("the warning repeated the credential: %q", errOut.String())
	}
}

func TestAddTargetStaysQuietWithoutCredentials(t *testing.T) {
	useDataDir(t, t.TempDir())

	var out, errOut bytes.Buffer
	cmd := newBackupCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"add-target", "--name", "nas", "--kind", "restic_local",
		"--uri", t.TempDir()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("add-target: %v", err)
	}

	if errOut.Len() != 0 {
		t.Errorf("a credential-free uri warned anyway: %q", errOut.String())
	}
}
