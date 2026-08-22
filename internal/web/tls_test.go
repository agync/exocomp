package web

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCertGeneratesAndPins(t *testing.T) {
	dir := t.TempDir()

	cert, fp, err := EnsureCert(dir)
	if err != nil {
		t.Fatalf("EnsureCert: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("certificate has no leaf")
	}
	if fp == "" {
		t.Fatal("no fingerprint")
	}

	// The private key must be 0600: it never leaves the box.
	info, err := os.Stat(filepath.Join(dir, keyFile))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key perm = %o, want 600", perm)
	}

	// A second call loads, not regenerates: the fingerprint an operator pinned
	// stays the same across restarts.
	_, fp2, err := EnsureCert(dir)
	if err != nil {
		t.Fatalf("EnsureCert second: %v", err)
	}
	if fp2 != fp {
		t.Fatalf("fingerprint changed across reload: %q -> %q", fp, fp2)
	}
}
