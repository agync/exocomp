package executor

import (
	"slices"
	"testing"
)

func TestRunSecretEnvRendersSortedPairs(t *testing.T) {
	t.Parallel()

	got, err := runSecretEnv(RunSecrets{
		"PROXMOX_USER":     "root@pam",
		"PROXMOX_PASSWORD": "s3cr3t",
		"TF_VAR_region":    "eu-central",
	})
	if err != nil {
		t.Fatalf("runSecretEnv: %v", err)
	}
	want := []string{
		"PROXMOX_PASSWORD=s3cr3t",
		"PROXMOX_USER=root@pam",
		"TF_VAR_region=eu-central",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("runSecretEnv = %v, want %v", got, want)
	}
}

func TestRunSecretEnvEmpty(t *testing.T) {
	t.Parallel()
	got, err := runSecretEnv(nil)
	if err != nil || got != nil {
		t.Fatalf("runSecretEnv(nil) = %v, %v", got, err)
	}
}

func TestRunSecretEnvRejectsBadNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"",             // empty
		"1LEADING",     // starts with a digit
		"HAS SPACE",    // whitespace
		"HAS=EQUALS",   // an "=" would smuggle a second variable
		"HAS-DASH",     // dash is not an identifier char
		"HAS\nNEWLINE", // newline
	} {
		if _, err := runSecretEnv(RunSecrets{name: "v"}); err == nil {
			t.Errorf("runSecretEnv accepted invalid env name %q", name)
		}
	}
}

func TestRunSecretEnvRejectsNULValue(t *testing.T) {
	t.Parallel()
	if _, err := runSecretEnv(RunSecrets{"TOKEN": "a\x00b"}); err == nil {
		t.Fatal("runSecretEnv accepted a value with a NUL")
	}
}
