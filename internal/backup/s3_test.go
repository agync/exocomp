package backup

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	minioImage    = "quay.io/minio/minio:latest"
	minioUser     = "exocomp-test-user"
	minioPassword = "exocomp-test-password"
	minioReady    = 90 * time.Second
)

// TestResticS3Leg runs the S3 leg against a real MinIO. It skips — with the
// reason — when podman cannot produce one, and never fakes the result.
func TestResticS3Leg(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman is not on PATH")
	}
	f := newFixture(t)
	endpoint := startMinIO(t)

	creds := map[string]string{
		"AWS_ACCESS_KEY_ID":     minioUser,
		"AWS_SECRET_ACCESS_KEY": minioPassword,
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

// startMinIO boots a throwaway MinIO on a loopback port and returns its
// endpoint URL.
func startMinIO(t *testing.T) string {
	t.Helper()

	port := freePort(t)
	name := fmt.Sprintf("exocomp-minio-%d", port)
	out, err := exec.Command("podman", "run", "--detach", "--rm", "--name", name,
		"--publish", fmt.Sprintf("127.0.0.1:%d:9000", port),
		"--env", "MINIO_ROOT_USER="+minioUser,
		"--env", "MINIO_ROOT_PASSWORD="+minioPassword,
		minioImage, "server", "/data").CombinedOutput()
	if err != nil {
		t.Skipf("podman run %s failed: %v: %s", minioImage, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		_ = exec.Command("podman", "rm", "--force", "--time", "1", name).Run()
	})

	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(minioReady)
	for time.Now().Before(deadline) {
		resp, err := http.Get(endpoint + "/minio/health/live")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return endpoint
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	logs, _ := exec.Command("podman", "logs", "--tail", "20", name).CombinedOutput()
	t.Skipf("minio did not become healthy within %s: %s", minioReady, strings.TrimSpace(string(logs)))
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
