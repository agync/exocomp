package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
)

func initForToken(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	dir := filepath.Join(root, "exocomp")
	env, _, err := secrets.Init(dir, nil)
	if err != nil {
		t.Fatalf("secrets.Init: %v", err)
	}
	_ = env.Close()
	return dir
}

func runToken(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newTokenCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestTokenNewListRevoke(t *testing.T) {
	initForToken(t)

	out, _, err := runToken(t, "new", "laptop", "--scope", "operator")
	if err != nil {
		t.Fatalf("token new: %v", err)
	}
	if !strings.Contains(out, "exo_") {
		t.Fatalf("token new did not print a token:\n%s", out)
	}
	if !strings.Contains(out, "certificate SHA-256") {
		t.Fatalf("token new did not print the certificate fingerprint:\n%s", out)
	}

	list, _, err := runToken(t, "list")
	if err != nil {
		t.Fatalf("token list: %v", err)
	}
	if !strings.Contains(list, "laptop") || !strings.Contains(list, "active") {
		t.Fatalf("token list missing the active token:\n%s", list)
	}
	// The listing never prints the secret, only a prefix handle.
	if strings.Contains(list, "exo_") {
		t.Fatalf("token list leaked a full token:\n%s", list)
	}

	if _, _, err := runToken(t, "revoke", "laptop"); err != nil {
		t.Fatalf("token revoke: %v", err)
	}

	list2, _, err := runToken(t, "list")
	if err != nil {
		t.Fatalf("token list after revoke: %v", err)
	}
	if strings.Contains(list2, "laptop") {
		t.Fatalf("revoked token still listed as active:\n%s", list2)
	}

	// A second revoke of the same name is a clean error, not a panic.
	if _, _, err := runToken(t, "revoke", "laptop"); err == nil {
		t.Fatal("revoking an already-revoked token should error")
	}
}

func TestTokenNewPrintsPairingQR(t *testing.T) {
	initForToken(t)

	out, _, err := runToken(t, "new", "pixel-8", "--scope", "operator")
	if err != nil {
		t.Fatalf("token new: %v", err)
	}
	if !strings.Contains(out, "Scan to pair a device") {
		t.Fatalf("token new did not offer a pairing QR:\n%s", out)
	}
	// The half-block QR must actually render module glyphs, not just a caption.
	if !strings.ContainsAny(out, "█▀▄") {
		t.Fatalf("pairing QR block is empty:\n%s", out)
	}
	for _, want := range []string{"url:", "fingerprint:", "scope:", "name:", "pixel-8"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pairing summary missing %q:\n%s", want, out)
		}
	}

	// --qr=false suppresses the QR while still minting.
	out, _, err = runToken(t, "new", "other", "--qr=false")
	if err != nil {
		t.Fatalf("token new --qr=false: %v", err)
	}
	if strings.Contains(out, "Scan to pair a device") {
		t.Fatalf("--qr=false still printed a QR:\n%s", out)
	}
}

func TestTokenNewRejectsBadScope(t *testing.T) {
	initForToken(t)
	if _, _, err := runToken(t, "new", "x", "--scope", "root"); err == nil {
		t.Fatal("an unknown scope must be refused")
	}
}
