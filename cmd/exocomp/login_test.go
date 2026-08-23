package main

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/client"
)

// withConfigHome points XDG_CONFIG_HOME at a temp dir so a test's login writes
// an isolated hosts.toml, never the operator's real one.
func withConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "exocomp", hostsFileName)
}

// loginTLSServer returns an httptest TLS server and its cert fingerprint.
func loginTLSServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	return srv, client.Fingerprint(srv.Certificate().Raw)
}

func runLoginCmd(t *testing.T, args []string, stdin string) (string, string, error) {
	t.Helper()
	root := newRootCmd()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errb.String(), err
}

func TestLoginPinsAndStores0600(t *testing.T) {
	path := withConfigHome(t)
	srv, fp := loginTLSServer(t)

	out, _, err := runLoginCmd(t,
		[]string{"login", srv.URL, "--name", "box", "--token", "exo_secret", "--assume-yes"}, "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(out, fp) {
		t.Errorf("login did not print the fingerprint:\n%s", out)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("hosts.toml not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != fs.FileMode(0o600) {
		t.Errorf("hosts.toml perms = %o, want 600", perm)
	}

	cfg, err := loadHostsConfig()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.Hosts["box"]
	if !ok {
		t.Fatalf("no stored entry: %+v", cfg)
	}
	if entry.Token != "exo_secret" || !sameFP(entry.Fingerprint, fp) {
		t.Errorf("stored entry wrong: %+v", entry)
	}
	if cfg.Default != "box" {
		t.Errorf("first login should adopt default, got %q", cfg.Default)
	}
}

func TestLoginTokenFromEnv(t *testing.T) {
	withConfigHome(t)
	srv, _ := loginTLSServer(t)
	t.Setenv(tokenEnv, "exo_from_env")

	if _, _, err := runLoginCmd(t, []string{"login", srv.URL, "--name", "box", "--assume-yes"}, ""); err != nil {
		t.Fatalf("login: %v", err)
	}
	cfg, _ := loadHostsConfig()
	if cfg.Hosts["box"].Token != "exo_from_env" {
		t.Errorf("token not taken from env: %+v", cfg.Hosts["box"])
	}
}

func TestLoginRequiresToken(t *testing.T) {
	withConfigHome(t)
	srv, _ := loginTLSServer(t)
	_, _, err := runLoginCmd(t, []string{"login", srv.URL, "--name", "box", "--assume-yes"}, "")
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("want no-token error, got %v", err)
	}
}

func TestLoginChangedFingerprintWarns(t *testing.T) {
	withConfigHome(t)

	// First login against server A pins its fingerprint.
	srvA, fpA := loginTLSServer(t)
	if _, _, err := runLoginCmd(t,
		[]string{"login", srvA.URL, "--name", "box", "--token", "t", "--assume-yes"}, ""); err != nil {
		t.Fatalf("first login: %v", err)
	}

	// A DIFFERENT server (server B) reachable under the SAME alias: a fresh cert,
	// so a different fingerprint. Re-login without --assume-yes must refuse.
	srvB := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer srvB.Close()
	fpB := client.Fingerprint(srvB.Certificate().Raw)
	if sameFP(fpA, fpB) {
		t.Skip("two httptest servers produced the same cert; cannot exercise the change path")
	}

	_, errOut, err := runLoginCmd(t,
		[]string{"login", srvB.URL, "--name", "box", "--token", "t"}, "y\n")
	if err == nil {
		t.Fatal("changed fingerprint must refuse without --assume-yes")
	}
	if !strings.Contains(errOut, "CHANGED") {
		t.Errorf("expected a loud CHANGED warning, got:\n%s", errOut)
	}

	// The stored pin must be UNCHANGED after the refusal (fail-closed).
	cfg, _ := loadHostsConfig()
	if !sameFP(cfg.Hosts["box"].Fingerprint, fpA) {
		t.Errorf("refused login must not overwrite the pin: %+v", cfg.Hosts["box"])
	}

	// With --assume-yes the re-key is accepted and the pin advances.
	if _, _, err := runLoginCmd(t,
		[]string{"login", srvB.URL, "--name", "box", "--token", "t", "--assume-yes"}, ""); err != nil {
		t.Fatalf("confirmed re-key: %v", err)
	}
	cfg, _ = loadHostsConfig()
	if !sameFP(cfg.Hosts["box"].Fingerprint, fpB) {
		t.Errorf("confirmed re-key should advance the pin: %+v", cfg.Hosts["box"])
	}
}

func TestLoginRefusedOnDecline(t *testing.T) {
	withConfigHome(t)
	srv, _ := loginTLSServer(t)
	// Interactive-looking stdin with a "no" answer; but SetIn is not a TTY, so
	// confirmPin refuses on non-interactive stdin regardless — either way nothing
	// is stored.
	_, _, err := runLoginCmd(t, []string{"login", srv.URL, "--name", "box", "--token", "t"}, "n\n")
	if err == nil {
		t.Fatal("expected refusal without --assume-yes on non-interactive stdin")
	}
	cfg, _ := loadHostsConfig()
	if _, ok := cfg.Hosts["box"]; ok {
		t.Error("declined login must store nothing")
	}
}

func TestDefaultAlias(t *testing.T) {
	cases := map[string]string{
		"nibbler.tailnet.ts.net:8443":         "nibbler",
		"https://nibbler.tailnet.ts.net:8443": "nibbler",
		"box":                                 "box",
		"10.0.0.5:8443":                       "10",
	}
	for in, want := range cases {
		got, err := defaultAlias(in)
		if err != nil {
			t.Errorf("defaultAlias(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("defaultAlias(%q) = %q, want %q", in, got, want)
		}
	}
}
