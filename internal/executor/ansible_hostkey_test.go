package executor_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
)

// hostKeyChangedBanner is the output an ansible run prints when a host's SSH key
// no longer matches the recorded pin: OpenSSH's changed-key banner surfaced
// inside ansible's UNREACHABLE result for the host.
const hostKeyChangedBanner = `fatal: [web1]: UNREACHABLE! => {"changed": false, "msg": "Failed to connect to the host via ssh: @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\n@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\nThe fingerprint for the ED25519 key sent by the remote host is\nSHA256:presented9d3f1anewfingerprintvalueAAAA.\nOffending ED25519 key in /home/exocomp/.ssh/known_hosts:3\nHost key verification failed.", "unreachable": true}`

// TestDetectHostKeyMismatch proves the parser pulls host, key type, presented
// fingerprint, and the known_hosts reference out of the banner.
func TestDetectHostKeyMismatch(t *testing.T) {
	t.Parallel()
	m, ok := executor.DetectHostKeyMismatch(hostKeyChangedBanner)
	if !ok {
		t.Fatal("mismatch banner not detected")
	}
	if m.Host != "web1" {
		t.Errorf("host = %q, want web1", m.Host)
	}
	if m.KeyType != "ED25519" {
		t.Errorf("key type = %q, want ED25519", m.KeyType)
	}
	if m.NewFingerprint != "SHA256:presented9d3f1anewfingerprintvalueAAAA" {
		t.Errorf("new fingerprint = %q", m.NewFingerprint)
	}
	if !strings.HasSuffix(m.KnownHostsRef, "known_hosts:3") {
		t.Errorf("known_hosts ref = %q", m.KnownHostsRef)
	}
}

// TestDetectHostKeyMismatchUnknownHost proves a first-contact unknown host
// (verification failed, but NOT the changed banner) is not classified as a
// re-pin: onboarding must not be mistaken for a security signal.
func TestDetectHostKeyMismatchUnknownHost(t *testing.T) {
	t.Parallel()
	unknown := `fatal: [web9]: UNREACHABLE! => {"msg": "Failed to connect to the host via ssh: Host key verification failed.", "unreachable": true}`
	if _, ok := executor.DetectHostKeyMismatch(unknown); ok {
		t.Fatal("an unknown-host failure must not be classified as a host-key change")
	}
}

// TestAnsibleRunSurfacesHostKeyMismatch is the 8d executor gate: a run whose
// connection hits a host-key mismatch surfaces it as a structured security
// event, while the run's exit code stays its verdict (invariant 3) and no
// driver error is manufactured.
func TestAnsibleRunSurfacesHostKeyMismatch(t *testing.T) {
	t.Parallel()
	// The fake ansible prints the changed-key banner and exits non-zero, as a
	// real run against a re-pinned host would.
	bin := fakeAnsible(t, "cat <<'EOF'\n"+hostKeyChangedBanner+"\nEOF\nexit 4\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var sink collector
	res, err := executor.NewAnsible(executor.AnsibleOptions{
		Binary:         bin,
		PrivateDirRoot: t.TempDir(),
	}).Run(ctx, executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv("/p/site.yml", "inventory.ini"),
	}, &sink)
	// Invariant 3: the exit code is the verdict, not a manufactured Go error.
	if err != nil {
		t.Fatalf("run returned a driver error for a nonzero exit: %v", err)
	}
	if res.ExitCode != 4 {
		t.Fatalf("exit code = %d, want 4 (preserved)", res.ExitCode)
	}

	var found executor.HostKeyMismatch
	var ok bool
	for _, raw := range sink.rawJSON() {
		var env struct {
			M *executor.HostKeyMismatch `json:"ansible_hostkey"`
		}
		if json.Unmarshal([]byte(raw), &env) == nil && env.M != nil {
			found, ok = *env.M, true
		}
	}
	if !ok {
		t.Fatalf("no ansible_hostkey security event surfaced: %v", sink.rawJSON())
	}
	if found.Host != "web1" || found.NewFingerprint != "SHA256:presented9d3f1anewfingerprintvalueAAAA" {
		t.Fatalf("host-key event lost detail: %+v", found)
	}

	// The human line reads as a security signal naming the host.
	var msg string
	for _, m := range sink.msgs(executor.LevelSystem) {
		if strings.HasPrefix(m, "SECURITY: host key changed") {
			msg = m
		}
	}
	if !strings.Contains(msg, "web1") {
		t.Errorf("security message = %q", msg)
	}
}

// TestAnsibleRunNoHostKeyEventOnCleanRun proves a normal run that never trips
// the banner surfaces no host-key event — the observer is transparent.
func TestAnsibleRunNoHostKeyEventOnCleanRun(t *testing.T) {
	t.Parallel()
	bin := fakeAnsible(t, "echo 'ok: [web1]'\nexit 0\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var sink collector
	if _, err := executor.NewAnsible(executor.AnsibleOptions{
		Binary:         bin,
		PrivateDirRoot: t.TempDir(),
	}).Run(ctx, executor.Spec{
		Kind: executor.KindAnsible,
		Argv: executor.AnsibleArgv("/p/site.yml", "inventory.ini"),
	}, &sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, raw := range sink.rawJSON() {
		if strings.Contains(raw, "ansible_hostkey") {
			t.Fatalf("a clean run surfaced a host-key event: %s", raw)
		}
	}
}
