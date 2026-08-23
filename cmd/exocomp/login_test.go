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
	srv, realFP := loginTLSServer(t)

	// Pre-seed the alias with a DIFFERENT (stale) fingerprint, standing in for a
	// previously-pinned cert. (httptest reuses one baked-in cert across servers,
	// so a second server cannot produce a genuinely different fingerprint — the
	// stale pin is injected directly instead.)
	staleFP := "00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF"
	if sameFP(staleFP, realFP) {
		t.Fatal("baked-in cert unexpectedly matched the stale fingerprint")
	}
	seed := hostsConfig{Hosts: map[string]hostEntry{
		"box": {Endpoint: srv.URL, Fingerprint: staleFP, Token: "t"},
	}}
	if err := saveHostsConfig(seed); err != nil {
		t.Fatal(err)
	}

	// Re-login: the presented cert differs from the pin, so without --replace-pin
	// it must warn loudly and refuse.
	_, errOut, err := runLoginCmd(t,
		[]string{"login", srv.URL, "--name", "box", "--token", "t"}, "y\n")
	if err == nil {
		t.Fatal("changed fingerprint must refuse without --replace-pin")
	}
	if !strings.Contains(errOut, "CHANGED") {
		t.Errorf("expected a loud CHANGED warning, got:\n%s", errOut)
	}

	// The stored pin must be UNCHANGED after the refusal (fail-closed).
	cfg, _ := loadHostsConfig()
	if !sameFP(cfg.Hosts["box"].Fingerprint, staleFP) {
		t.Errorf("refused login must not overwrite the pin: %+v", cfg.Hosts["box"])
	}

	// With --replace-pin the re-key is accepted and the pin advances to the real one.
	if _, _, err := runLoginCmd(t,
		[]string{"login", srv.URL, "--name", "box", "--token", "t", "--replace-pin"}, ""); err != nil {
		t.Fatalf("confirmed re-key: %v", err)
	}
	cfg, _ = loadHostsConfig()
	if !sameFP(cfg.Hosts["box"].Fingerprint, realFP) {
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

// TestLoginAssumeYesCannotReplaceAPin is the AI-5 regression: --assume-yes is a
// root persistent flag every script and agent harness must pass to pin at all on
// non-interactive stdin, so it must NOT double as consent to overwrite a CHANGED
// fingerprint. A machine-in-the-middle answering the endpoint gets a refusal and
// an untouched hosts.toml; replacing a pin takes the separate --replace-pin.
func TestLoginAssumeYesCannotReplaceAPin(t *testing.T) {
	withConfigHome(t)
	srv, realFP := loginTLSServer(t)

	staleFP := "00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF"
	seed := hostsConfig{Hosts: map[string]hostEntry{
		"box": {Endpoint: srv.URL, Fingerprint: staleFP, Token: "old-token"},
	}}
	if err := saveHostsConfig(seed); err != nil {
		t.Fatal(err)
	}

	_, errOut, err := runLoginCmd(t,
		[]string{"login", srv.URL, "--name", "box", "--token", "new-token", "--assume-yes"}, "")
	if err == nil {
		t.Fatal("--assume-yes must not overwrite a changed fingerprint")
	}
	if !strings.Contains(errOut, "CHANGED") {
		t.Errorf("expected a loud CHANGED warning, got:\n%s", errOut)
	}
	cfg, _ := loadHostsConfig()
	if !sameFP(cfg.Hosts["box"].Fingerprint, staleFP) || cfg.Hosts["box"].Token != "old-token" {
		t.Fatalf("refused login rewrote the entry: %+v", cfg.Hosts["box"])
	}

	// The deliberate, login-only consent replaces it.
	if _, _, err := runLoginCmd(t,
		[]string{"login", srv.URL, "--name", "box", "--token", "new-token", "--replace-pin"}, ""); err != nil {
		t.Fatalf("--replace-pin re-key: %v", err)
	}
	cfg, _ = loadHostsConfig()
	if !sameFP(cfg.Hosts["box"].Fingerprint, realFP) || cfg.Hosts["box"].Token != "new-token" {
		t.Errorf("--replace-pin should advance the pin: %+v", cfg.Hosts["box"])
	}
}

// TestLoginRefusesAliasEndpointChange: an alias already pinned to one endpoint
// is not silently re-pointed at another host by a login that reuses its name
// (AI-5's second half) — the same deliberate consent is required.
func TestLoginRefusesAliasEndpointChange(t *testing.T) {
	withConfigHome(t)
	srv, fp := loginTLSServer(t)

	seed := hostsConfig{Hosts: map[string]hostEntry{
		"box": {Endpoint: "https://elsewhere.example:8443", Fingerprint: fp, Token: "old-token"},
	}}
	if err := saveHostsConfig(seed); err != nil {
		t.Fatal(err)
	}

	_, _, err := runLoginCmd(t,
		[]string{"login", srv.URL, "--name", "box", "--token", "new-token", "--assume-yes"}, "")
	if err == nil {
		t.Fatal("reusing an alias for a different endpoint must refuse")
	}
	cfg, _ := loadHostsConfig()
	if cfg.Hosts["box"].Endpoint != "https://elsewhere.example:8443" {
		t.Errorf("refused login rewrote the endpoint: %+v", cfg.Hosts["box"])
	}
}
