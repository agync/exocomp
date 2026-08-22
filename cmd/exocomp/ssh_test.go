package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/spf13/cobra"
)

// TestSSHServeRendersTUIOverPublicKeyAuth is the scripted probe the M2a
// hardening gate asks for: a real `ssh` client, a real generated key pair, a
// real authorized_keys file, against a real ssh-serve instance — proving the
// TUI actually renders over the wish/bubbletea-v2 pairing rather than just
// that the server accepts a TCP connection.
func TestSSHServeRendersTUIOverPublicKeyAuth(t *testing.T) {
	requireSSHTools(t)
	dir := t.TempDir()
	useDataDir(t, dir)

	goodKey := genTestKey(t, dir, "good")
	authorizedKeys := writeAuthorizedKeys(t, dir, goodKey+".pub")

	addr := "127.0.0.1:" + freeSSHPort(t)
	stop := startSSHServe(t, addr, authorizedKeys)
	defer stop()
	waitForListener(t, addr)

	out, status := sshProbe(t, addr, goodKey, "q")
	if status != 0 {
		t.Fatalf("ssh session exited %d, transcript:\n%s", status, out)
	}
	// The TUI opens on the Home glance ([0], the root): its header, the
	// needs/queue sections, and the screen-selector help bar.
	for _, want := range []string{"exocomp", "NEEDS YOU", "QUEUE", "BACKUPS", "[0-9] screens", "[q] quit"} {
		if !strings.Contains(out, want) {
			t.Errorf("session transcript does not contain %q:\n%s", want, out)
		}
	}
}

// TestSSHServeRejectsWrongKey proves the server authenticates by public key
// against exactly the authorized_keys file — a key that is not on it, valid
// key pair or not, never reaches a session.
func TestSSHServeRejectsWrongKey(t *testing.T) {
	requireSSHTools(t)
	dir := t.TempDir()
	useDataDir(t, dir)

	goodKey := genTestKey(t, dir, "good")
	badKey := genTestKey(t, dir, "bad")
	authorizedKeys := writeAuthorizedKeys(t, dir, goodKey+".pub")

	addr := "127.0.0.1:" + freeSSHPort(t)
	stop := startSSHServe(t, addr, authorizedKeys)
	defer stop()
	waitForListener(t, addr)

	_, port, _ := net.SplitHostPort(addr)
	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "ConnectTimeout=5",
		"-i", badKey,
		"-p", port,
		"127.0.0.1", "--", "true",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		t.Fatal("ssh accepted a key that is not in authorized_keys")
	}
	if !strings.Contains(out.String(), "Permission denied") {
		t.Errorf("ssh output = %q, want a publickey permission refusal", out.String())
	}
}

// startSSHServe runs the real ssh-serve command in this test binary — same
// code path as `exocomp ssh-serve` — and returns a function that shuts it
// down and waits for it to exit.
func startSSHServe(t *testing.T, addr, authorizedKeys string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	done := make(chan error, 1)
	go func() { done <- runSSHServe(cmd, addr, authorizedKeys) }()

	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ssh-serve exited with an error: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Error("ssh-serve did not shut down within 5s")
		}
	}
}

// sshProbe drives a real ssh client against a pty it allocates itself — the
// test process has no controlling terminal of its own — sends keys after
// giving the remote TUI a moment to render, and returns everything the
// session printed plus the client's exit status.
func sshProbe(t *testing.T, addr, keyPath, keys string) (string, int) {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %s: %v", addr, err)
	}

	cmd := exec.Command("ssh",
		"-tt",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "ConnectTimeout=5",
		"-i", keyPath,
		"-p", port,
		"127.0.0.1",
	)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("allocate a pty for ssh: %v", err)
	}
	defer func() { _ = f.Close() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	var buf bytes.Buffer
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, f)
		close(copyDone)
	}()

	// Give Init() and the first render a moment before the quit key lands —
	// sending it immediately can race the TUI's first paint.
	time.Sleep(1500 * time.Millisecond)
	if _, err := f.Write([]byte(keys)); err != nil {
		t.Fatalf("write %q to the session: %v", keys, err)
	}

	select {
	case <-copyDone:
	case <-time.After(8 * time.Second):
		t.Fatal("ssh session did not end within 8s of the quit key")
	}

	waitErr := cmd.Wait()
	status := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		status = exitErr.ExitCode()
	} else if waitErr != nil {
		t.Fatalf("ssh wait: %v", waitErr)
	}
	return buf.String(), status
}

func genTestKey(t *testing.T, dir, name string) (privPath string) {
	t.Helper()
	priv := filepath.Join(dir, name)
	out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", priv, "-q").CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	return priv
}

func writeAuthorizedKeys(t *testing.T, dir, pubKeyPath string) string {
	t.Helper()
	body, err := os.ReadFile(pubKeyPath)
	if err != nil {
		t.Fatalf("read %s: %v", pubKeyPath, err)
	}
	path := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func freeSSHPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return strconv.Itoa(port)
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("ssh-serve never started listening on %s", addr)
}

func requireSSHTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is required for this test and is not on PATH", name)
		}
	}
}
