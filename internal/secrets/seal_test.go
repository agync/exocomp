package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTransit emulates the OpenBao Transit encrypt/decrypt endpoints with a
// reversible transform (base64 under a vault:v1: prefix). It is NOT real
// crypto — it exists so a unit test round-trips the DEK through the exact HTTP
// shape without a live OpenBao. It records the token it was called with so a
// test can assert the token travelled in the header, never in a logged place.
type fakeTransit struct {
	server   *httptest.Server
	keyName  string
	token    string
	gotToken string
	calls    int
}

func newFakeTransit(t *testing.T, keyName, token string) *fakeTransit {
	t.Helper()
	f := &fakeTransit{keyName: keyName, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/transit/encrypt/"+keyName, func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.gotToken = r.Header.Get("X-Vault-Token")
		if f.gotToken != token {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
			return
		}
		var in struct {
			Plaintext string `json:"plaintext"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"errors":["bad body"]}`, http.StatusBadRequest)
			return
		}
		ct := "vault:v1:" + in.Plaintext
		writeJSON(w, map[string]any{"data": map[string]string{"ciphertext": ct}})
	})
	mux.HandleFunc("/v1/transit/decrypt/"+keyName, func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.gotToken = r.Header.Get("X-Vault-Token")
		if f.gotToken != token {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
			return
		}
		var in struct {
			Ciphertext string `json:"ciphertext"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"errors":["bad body"]}`, http.StatusBadRequest)
			return
		}
		pt := strings.TrimPrefix(in.Ciphertext, "vault:v1:")
		writeJSON(w, map[string]any{"data": map[string]string{"plaintext": pt}})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeTransit) config() *SealConfig {
	return &SealConfig{
		Address:    f.server.URL,
		Token:      f.token,
		KeyName:    f.keyName,
		HTTPClient: f.server.Client(),
	}
}

func initEnvelope(t *testing.T) (string, *Envelope) {
	t.Helper()
	dir := t.TempDir()
	env, _, err := Init(dir, nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return dir, env
}

// TestTransitSealRoundTrip seals the DEK against the fake transit server and
// proves OpenWithSeal recovers the identical DEK — a derived subkey matches.
func TestTransitSealRoundTrip(t *testing.T) {
	dir, env := initEnvelope(t)
	want := env.Derive("restic-repo", "nas")
	if len(want) != dekLen {
		t.Fatalf("derive returned %d bytes", len(want))
	}

	fake := newFakeTransit(t, "exocomp", "s.sekret-token")
	seal := fake.config()
	if err := EnableTransitSeal(context.Background(), env, seal); err != nil {
		t.Fatalf("EnableTransitSeal: %v", err)
	}
	if err := env.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !TransitSealPresent(dir) {
		t.Fatalf("keyring.transit was not written")
	}

	opened, err := OpenWithSeal(dir, seal)
	if err != nil {
		t.Fatalf("OpenWithSeal (transit): %v", err)
	}
	defer func() { _ = opened.Close() }()

	got := opened.Derive("restic-repo", "nas")
	if !bytes.Equal(want, got) {
		t.Fatalf("transit-opened DEK differs: derived subkey mismatch")
	}
	if fake.calls < 2 {
		t.Fatalf("expected an encrypt and a decrypt call, got %d", fake.calls)
	}
	if fake.gotToken != "s.sekret-token" {
		t.Fatalf("token did not travel in the header")
	}
}

// TestUnconfiguredPathUnchanged is the backward-compat regression: with a nil
// seal, OpenWithSeal is byte-for-byte Open — no transit file, the same on-disk
// layout, the same DEK — and enabling a seal never mutates keyring.age.
func TestUnconfiguredPathUnchanged(t *testing.T) {
	dir, env := initEnvelope(t)
	want := env.Derive("tofu-state", "homelab")
	keyringBefore := readFile(t, filepath.Join(dir, keyringFile))
	layoutBefore := listDir(t, dir)
	if err := env.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// nil seal: identical to Open.
	opened, err := OpenWithSeal(dir, nil)
	if err != nil {
		t.Fatalf("OpenWithSeal(nil): %v", err)
	}
	got := opened.Derive("tofu-state", "homelab")
	_ = opened.Close()
	if !bytes.Equal(want, got) {
		t.Fatalf("nil-seal DEK differs from Init DEK")
	}
	if !equalStrings(listDir(t, dir), layoutBefore) {
		t.Fatalf("nil-seal Open changed the on-disk layout: %v -> %v", layoutBefore, listDir(t, dir))
	}

	// An unconfigured SealConfig behaves the same as nil.
	if (&SealConfig{Address: "https://x"}).Configured() {
		t.Fatalf("a config missing token+key must not be Configured")
	}

	// Enabling a seal is additive: keyring.age is untouched.
	fake := newFakeTransit(t, "exocomp", "tok")
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := EnableTransitSeal(context.Background(), reopened, fake.config()); err != nil {
		t.Fatalf("EnableTransitSeal: %v", err)
	}
	_ = reopened.Close()
	if !bytes.Equal(keyringBefore, readFile(t, filepath.Join(dir, keyringFile))) {
		t.Fatalf("enabling a transit seal mutated keyring.age")
	}
	// And the age recovery path still works with the seal file present.
	if _, err := Open(dir); err != nil {
		t.Fatalf("Open after seal enable (host-key path): %v", err)
	}
}

// TestUnreachableOpenBaoIsExplicit proves a configured-but-unreachable seal
// fails Open with a clear error and does NOT silently fall back to the host-key
// path, even though a valid host.key that would open the store is present.
func TestUnreachableOpenBaoIsExplicit(t *testing.T) {
	dir, env := initEnvelope(t)
	fake := newFakeTransit(t, "exocomp", "tok")
	seal := fake.config()
	if err := EnableTransitSeal(context.Background(), env, seal); err != nil {
		t.Fatalf("EnableTransitSeal: %v", err)
	}
	_ = env.Close()

	// Point the seal at the now-closed server: connection refused.
	fake.server.Close()

	_, err := OpenWithSeal(dir, seal)
	if err == nil {
		t.Fatalf("expected an explicit error for an unreachable OpenBao, got nil (silent fallback)")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("error does not name the unreachable seal: %v", err)
	}
	// The host key alone WOULD open it — prove the fallback did not happen by
	// confirming the plain Open path still succeeds.
	if opened, oerr := Open(dir); oerr != nil {
		t.Fatalf("host-key Open should still work: %v", oerr)
	} else {
		_ = opened.Close()
	}
}

// TestSealMissingBlobIsExplicit: seal configured but no keyring.transit yet.
func TestSealMissingBlobIsExplicit(t *testing.T) {
	dir, env := initEnvelope(t)
	_ = env.Close()
	fake := newFakeTransit(t, "exocomp", "tok")
	_, err := OpenWithSeal(dir, fake.config())
	if err == nil || !strings.Contains(err.Error(), TransitSealName) {
		t.Fatalf("expected an explicit missing-blob error naming %s, got %v", TransitSealName, err)
	}
}

// TestNoSecretsLeak: the on-disk transit blob is transit ciphertext, not the raw
// DEK, and an unreachable error never carries the token.
func TestNoSecretsLeak(t *testing.T) {
	dir, env := initEnvelope(t)
	dekCopy := append([]byte(nil), env.dek...)
	fake := newFakeTransit(t, "exocomp", "top-secret-token")
	seal := fake.config()
	if err := EnableTransitSeal(context.Background(), env, seal); err != nil {
		t.Fatalf("EnableTransitSeal: %v", err)
	}
	_ = env.Close()

	blob := readFile(t, filepath.Join(dir, transitSealFile))
	if bytes.Contains(blob, dekCopy) {
		t.Fatalf("transit blob contains the raw DEK")
	}
	if bytes.Contains(blob, []byte("top-secret-token")) {
		t.Fatalf("transit blob contains the token")
	}
	if !bytes.HasPrefix(blob, []byte("vault:v1:")) {
		t.Fatalf("transit blob is not a transit ciphertext: %q", blob)
	}

	fake.server.Close()
	_, err := OpenWithSeal(dir, seal)
	if err == nil {
		t.Fatalf("expected unreachable error")
	}
	if strings.Contains(err.Error(), "top-secret-token") {
		t.Fatalf("error message leaked the token: %v", err)
	}
	// The base64 DEK must not appear either.
	if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(dekCopy)) {
		t.Fatalf("error message leaked the DEK")
	}
}

func TestSealConfigFromEnv(t *testing.T) {
	// Nothing set: unconfigured, no error.
	clearSealEnv(t)
	cfg, err := SealConfigFromEnv()
	if err != nil || cfg != nil {
		t.Fatalf("unset env: want (nil,nil), got (%v,%v)", cfg, err)
	}

	// Partial: address only -> explicit error.
	t.Setenv(EnvSealAddress, "https://bao.lan:8200")
	if _, err := SealConfigFromEnv(); err == nil {
		t.Fatalf("partial config must error")
	}

	// Complete via token file.
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("s.filetoken\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	t.Setenv(EnvSealKey, "exocomp")
	t.Setenv(EnvSealTokenFile, tokenPath)
	cfg, err = SealConfigFromEnv()
	if err != nil {
		t.Fatalf("complete config: %v", err)
	}
	if !cfg.Configured() || cfg.Token != "s.filetoken" || cfg.KeyName != "exocomp" {
		t.Fatalf("config not resolved from env+file: %+v", cfg)
	}
}

func clearSealEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvSealAddress, EnvSealToken, EnvSealTokenFile, EnvSealKey, EnvSealNamespace} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
