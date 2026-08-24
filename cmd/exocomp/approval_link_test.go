package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/store"
)

// TestApprovalLinkPrefersPublicURL: `serve --public-url` roots the PUSH
// decision-link path at a reachable address (serve.go's engine.SetBaseURL),
// but that value lives only in the running daemon's memory — a separate
// `approval link` invocation must be told it explicitly to mint the same
// reachable link instead of the transient loopback address.
func TestApprovalLinkPrefersPublicURL(t *testing.T) {
	dir, _ := initForKit(t)
	st, approval := seedPendingApproval(t, "homelab")
	defer func() { _ = st.Close() }()

	// A loopback address IS recorded, as a real `serve` run would leave one —
	// proving --public-url is preferred over it, not just used for its absence.
	if err := api.WriteAddress(filepath.Join(dir, api.AddressFile), "127.0.0.1:54321"); err != nil {
		t.Fatalf("seed address file: %v", err)
	}

	out, err := runCobra(t, newApprovalCmd, "link", strconv.FormatInt(approval.ID, 10),
		"--public-url", "https://exocomp.example.ts.net:8443")
	if err != nil {
		t.Fatalf("approval link --public-url: %v (%s)", err, out)
	}
	if !strings.Contains(out, "https://exocomp.example.ts.net:8443/approval/") {
		t.Fatalf("link did not use --public-url:\n%s", out)
	}
	if strings.Contains(out, "127.0.0.1:54321") {
		t.Fatalf("link fell back to the loopback address despite --public-url:\n%s", out)
	}
}

// TestApprovalLinkPublicURLViaEnv proves the env-var counterpart works the
// same way as the flag, for a caller that exported it once alongside `serve
// --public-url` rather than repeating it on every invocation.
func TestApprovalLinkPublicURLViaEnv(t *testing.T) {
	dir, _ := initForKit(t)
	st, approval := seedPendingApproval(t, "homelab")
	defer func() { _ = st.Close() }()

	if err := api.WriteAddress(filepath.Join(dir, api.AddressFile), "127.0.0.1:54321"); err != nil {
		t.Fatalf("seed address file: %v", err)
	}
	t.Setenv(approvalPublicURLEnv, "https://exocomp.example.ts.net:8443")

	out, err := runCobra(t, newApprovalCmd, "link", strconv.FormatInt(approval.ID, 10))
	if err != nil {
		t.Fatalf("approval link: %v (%s)", err, out)
	}
	if !strings.Contains(out, "https://exocomp.example.ts.net:8443/approval/") {
		t.Fatalf("link did not use %s:\n%s", approvalPublicURLEnv, out)
	}
}

// TestApprovalLinkUsesPersistedDaemonPublicURL proves the follow-up to the
// creds-redact lane: a running `serve --public-url` daemon persists that base
// as a store setting, so a separate `approval link` invocation with neither
// the flag nor the env var set still mints a reachable link instead of
// falling back to the recorded (unreachable off-box) loopback address.
func TestApprovalLinkUsesPersistedDaemonPublicURL(t *testing.T) {
	dir, _ := initForKit(t)
	st, approval := seedPendingApproval(t, "homelab")
	defer func() { _ = st.Close() }()

	if err := api.WriteAddress(filepath.Join(dir, api.AddressFile), "127.0.0.1:54321"); err != nil {
		t.Fatalf("seed address file: %v", err)
	}
	if err := st.PutSetting(context.Background(), store.SettingPublicURL,
		"https://exocomp.example.ts.net:8443"); err != nil {
		t.Fatalf("seed persisted public-url setting: %v", err)
	}

	out, err := runCobra(t, newApprovalCmd, "link", strconv.FormatInt(approval.ID, 10))
	if err != nil {
		t.Fatalf("approval link: %v (%s)", err, out)
	}
	if !strings.Contains(out, "https://exocomp.example.ts.net:8443/approval/") {
		t.Fatalf("link did not use the persisted daemon public-url:\n%s", out)
	}
	if strings.Contains(out, "127.0.0.1:54321") {
		t.Fatalf("link fell back to the loopback address despite a persisted public-url:\n%s", out)
	}
}

// TestApprovalLinkFallsBackToLoopback: with neither --public-url nor the env
// var set, the link still falls back to the daemon's recorded address —
// unreachable off-box, but the documented, unchanged default.
func TestApprovalLinkFallsBackToLoopback(t *testing.T) {
	dir, _ := initForKit(t)
	st, approval := seedPendingApproval(t, "homelab")
	defer func() { _ = st.Close() }()

	if err := api.WriteAddress(filepath.Join(dir, api.AddressFile), "127.0.0.1:54321"); err != nil {
		t.Fatalf("seed address file: %v", err)
	}

	out, err := runCobra(t, newApprovalCmd, "link", strconv.FormatInt(approval.ID, 10))
	if err != nil {
		t.Fatalf("approval link: %v (%s)", err, out)
	}
	if !strings.Contains(out, "http://127.0.0.1:54321/approval/") {
		t.Fatalf("link did not fall back to the recorded loopback address:\n%s", out)
	}
}
