package backup

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/store"
)

// SeaweedFS (Apache-2.0, active) replaced the MinIO fixture the S3 leg was
// pinned to (docs/sbom/minio-assessment.md: MinIO CE is abandoned upstream,
// not a license problem). Verified compatible: restic init/backup/restore,
// this package's own SigV4 client for keyring.age/RECOVERY.md at the bucket
// root, and sealed-credential resolution all pass against it unchanged.
const (
	seaweedfsImage    = "docker.io/chrislusf/seaweedfs:latest"
	seaweedfsUser     = "exocomp-test-user"
	seaweedfsPassword = "exocomp-test-password"
	seaweedfsReady    = 90 * time.Second
)

// TestResticS3Leg runs the S3 leg against a real SeaweedFS. It skips — with
// the reason — when podman cannot produce one, and never fakes the result.
func TestResticS3Leg(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	t.Parallel()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman is not on PATH")
	}
	f := newFixture(t)
	endpoint := startSeaweedFS(t)

	creds := map[string]string{
		"AWS_ACCESS_KEY_ID":     seaweedfsUser,
		"AWS_SECRET_ACCESS_KEY": seaweedfsPassword,
		"AWS_DEFAULT_REGION":    "us-east-1",
		"RESTIC_CACHE_DIR":      f.cache,
	}
	uri := "s3:" + endpoint + "/exocomp-backup"
	target := f.target(t, "offsite-s3", KindResticS3, uri)
	target.Env = creds

	report, err := f.engine.Run(t.Context(), []Target{target})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := report.Err(); err != nil {
		t.Fatalf("s3 leg: %v", err)
	}
	if report.Legs[0].SnapshotID == "" {
		t.Error("no snapshot id parsed")
	}

	// keyring.age and RECOVERY.md are objects at the bucket root, outside the
	// restic repository's own key space.
	client, err := newS3Client(uri, creds)
	if err != nil {
		t.Fatalf("s3 client: %v", err)
	}
	keyring, err := client.get(t.Context(), KeyringName)
	if err != nil {
		t.Fatalf("get %s: %v", KeyringName, err)
	}
	want, err := f.env.KeyringAge()
	if err != nil {
		t.Fatalf("KeyringAge: %v", err)
	}
	if !bytes.Equal(keyring, want) {
		t.Error("the keyring at the bucket root is not the envelope's")
	}
	recovery, err := client.get(t.Context(), RecoveryName)
	if err != nil {
		t.Fatalf("get %s: %v", RecoveryName, err)
	}
	if got := targetNameFromRecovery(recovery); got != "offsite-s3" {
		t.Errorf("target name from RECOVERY.md = %q, want offsite-s3", got)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreLatest(t.Context(), Source{
		Kind:         KindResticS3,
		URI:          uri,
		IdentityFile: f.identity,
		Env:          creds,
	}, dest); err != nil {
		t.Fatalf("restore from s3: %v", err)
	}
	assertRestoredChain(t, f, dest, "offsite-s3")
}

// TestResticS3LegResolvesSealedCredentials proves the leg succeeds from
// sealed secret rows alone: the target's own Env carries only
// RESTIC_CACHE_DIR, so AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY can only
// have reached restic and the s3 client through resolveS3Env.
func TestResticS3LegResolvesSealedCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	t.Parallel()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman is not on PATH")
	}
	f := newFixture(t)
	endpoint := startSeaweedFS(t)

	const targetName = "offsite-s3-sealed"
	sealS3Secret(t, f, targetName, "access-key-id", seaweedfsUser)
	sealS3Secret(t, f, targetName, "secret-access-key", seaweedfsPassword)
	sealS3Secret(t, f, targetName, "region", "us-east-1")

	uri := "s3:" + endpoint + "/exocomp-backup-sealed"
	target := f.target(t, targetName, KindResticS3, uri)

	report, err := f.engine.Run(t.Context(), []Target{target})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := report.Err(); err != nil {
		t.Fatalf("s3 leg with sealed credentials: %v", err)
	}
	if report.Legs[0].SnapshotID == "" {
		t.Error("no snapshot id parsed")
	}

	client, err := newS3Client(uri, map[string]string{
		"AWS_ACCESS_KEY_ID":     seaweedfsUser,
		"AWS_SECRET_ACCESS_KEY": seaweedfsPassword,
		"AWS_DEFAULT_REGION":    "us-east-1",
	})
	if err != nil {
		t.Fatalf("s3 client: %v", err)
	}
	if _, err := client.get(t.Context(), KeyringName); err != nil {
		t.Fatalf("get %s written under sealed credentials: %v", KeyringName, err)
	}
}

func sealS3Secret(t *testing.T, f *fixture, target, field, value string) {
	t.Helper()
	name := s3SecretName(target, field)
	sealed, err := f.env.Seal(name, "s3-credential", []byte(value))
	if err != nil {
		t.Fatalf("seal %s: %v", name, err)
	}
	if _, err := f.store.CreateSecret(t.Context(), store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		AAD: sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		t.Fatalf("create secret row %s: %v", name, err)
	}
}

// TestResolveS3EnvPrefersSealedCredentials covers resolveS3Env directly:
// sealed rows win over fallback, an incomplete pair (only one of the two
// required fields) falls back entirely rather than handing restic a half
// credential, and either path carries fallback-only keys like
// RESTIC_CACHE_DIR through untouched.
func TestResolveS3EnvPrefersSealedCredentials(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fallback := map[string]string{
		"AWS_ACCESS_KEY_ID":     "fallback-key",
		"AWS_SECRET_ACCESS_KEY": "fallback-secret",
		"RESTIC_CACHE_DIR":      "/cache",
	}

	t.Run("no sealed rows falls back and warns", func(t *testing.T) {
		env, warned, err := resolveS3Env(t.Context(), f.store, f.env, "no-secrets", fallback)
		if err != nil {
			t.Fatalf("resolveS3Env: %v", err)
		}
		if !warned {
			t.Error("warned = false, want true when no rows are sealed")
		}
		if env["AWS_ACCESS_KEY_ID"] != "fallback-key" || env["RESTIC_CACHE_DIR"] != "/cache" {
			t.Errorf("env = %+v, want the fallback map unchanged", env)
		}
	})

	t.Run("sealed rows win and do not warn", func(t *testing.T) {
		sealS3Secret(t, f, "sealed-target", "access-key-id", "sealed-key")
		sealS3Secret(t, f, "sealed-target", "secret-access-key", "sealed-secret")

		env, warned, err := resolveS3Env(t.Context(), f.store, f.env, "sealed-target", fallback)
		if err != nil {
			t.Fatalf("resolveS3Env: %v", err)
		}
		if warned {
			t.Error("warned = true, want false when both required rows are sealed")
		}
		if env["AWS_ACCESS_KEY_ID"] != "sealed-key" || env["AWS_SECRET_ACCESS_KEY"] != "sealed-secret" {
			t.Errorf("env = %+v, want the sealed credential", env)
		}
		if env["RESTIC_CACHE_DIR"] != "/cache" {
			t.Errorf("env = %+v, want the fallback-only key carried through", env)
		}
		if _, ok := env["AWS_DEFAULT_REGION"]; ok {
			t.Errorf("env = %+v, want no region when none was sealed", env)
		}
	})

	t.Run("incomplete pair falls back entirely", func(t *testing.T) {
		sealS3Secret(t, f, "half-sealed-target", "access-key-id", "sealed-key")

		env, warned, err := resolveS3Env(t.Context(), f.store, f.env, "half-sealed-target", fallback)
		if err != nil {
			t.Fatalf("resolveS3Env: %v", err)
		}
		if !warned {
			t.Error("warned = false, want true for an incomplete sealed pair")
		}
		if env["AWS_ACCESS_KEY_ID"] != "fallback-key" {
			t.Errorf("env = %+v, want the fallback key, not a half-sealed credential", env)
		}
	})
}

// startSeaweedFS boots a throwaway SeaweedFS S3 gateway on a loopback port
// and returns its endpoint URL. SeaweedFS rejects a signed request outright
// without an explicit identity config (verified: anonymous mode makes restic
// hang retrying "Signed request requires setting up SeaweedFS S3
// authentication"), so this writes one naming seaweedfsUser/seaweedfsPassword
// and mounts it in.
func startSeaweedFS(t *testing.T) string {
	t.Helper()

	// t.TempDir() and 0600 both fail here: rootless podman's user-namespace
	// UID remap means the container's own user is neither this directory's
	// nor this file's owner, so both need to be world-readable (verified —
	// 0600 fails "permission denied" reading the bind-mounted config). It
	// holds only this throwaway fixture's dummy credential, not a secret.
	configDir := t.TempDir()
	if err := os.Chmod(configDir, 0o755); err != nil {
		t.Fatalf("chmod seaweedfs config dir: %v", err)
	}
	config := fmt.Sprintf(`{
  "identities": [
    {
      "name": %[1]q,
      "credentials": [{"accessKey": %[1]q, "secretKey": %[2]q}],
      "actions": ["Admin", "Read", "Write"]
    }
  ]
}
`, seaweedfsUser, seaweedfsPassword)
	if err := os.WriteFile(filepath.Join(configDir, "s3.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("write seaweedfs s3 config: %v", err)
	}

	port := freePort(t)
	name := fmt.Sprintf("exocomp-seaweedfs-%d", port)
	out, err := exec.Command("podman", "run", "--detach", "--rm", "--name", name,
		"--publish", fmt.Sprintf("127.0.0.1:%d:8333", port),
		"--volume", configDir+":/etc/seaweedfs:Z",
		seaweedfsImage, "server", "-s3", "-s3.port=8333", "-s3.config=/etc/seaweedfs/s3.json",
		"-dir=/data").CombinedOutput()
	if err != nil {
		t.Skipf("podman run %s failed: %v: %s", seaweedfsImage, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		_ = exec.Command("podman", "rm", "--force", "--time", "1", name).Run()
	})

	// SeaweedFS has no /minio-style health endpoint, and with a real identity
	// config an unauthenticated GET answers 403 rather than 200 (verified) —
	// so readiness is "the S3 API is answering HTTP at all", any status.
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(seaweedfsReady)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(endpoint + "/"); err == nil {
			_ = resp.Body.Close()
			return endpoint
		}
		time.Sleep(250 * time.Millisecond)
	}
	logs, _ := exec.Command("podman", "logs", "--tail", "20", name).CombinedOutput()
	t.Skipf("seaweedfs did not become healthy within %s: %s", seaweedfsReady, strings.TrimSpace(string(logs)))
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}
