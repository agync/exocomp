package deps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func countUVLinesContaining(t *testing.T, counter, marker string) int {
	t.Helper()
	body, err := os.ReadFile(counter)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read counter: %v", err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.Contains(line, marker) {
			n++
		}
	}
	return n
}

// TestEnsureVenvSerialisesConcurrentProvision: N concurrent callers for one
// cold venv invoke the fake uv venv step exactly once and all receive a valid,
// marker-complete venv.
func TestEnsureVenvSerialisesConcurrentProvision(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	counter := filepath.Join(t.TempDir(), "uv-invocations")
	uv := fakeUVDelayed(t, counter, "0.2")

	m := manifestFor(t, project, "jmespath==1.1.0\n")
	spec := VenvSpec{CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv}

	const N = 8
	var wg sync.WaitGroup
	results := make([]Venv, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := EnsureVenv(context.Background(), spec)
			results[i] = v
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if _, err := os.Stat(results[i].marker()); err != nil {
			t.Fatalf("caller %d: ready marker absent: %v", i, err)
		}
		if results[i].Dir == "" {
			t.Fatalf("caller %d: empty dir", i)
		}
	}
	if got := countUVLinesContaining(t, counter, "venv"); got != 1 {
		t.Errorf("uv venv step invoked %d times, want 1", got)
	}
	if got := countUVLinesContaining(t, counter, "pip install"); got != 1 {
		t.Errorf("uv pip install invoked %d times, want 1", got)
	}
}

// TestEnsureVenvWaiterHonoursCtxCancellation: a waiter whose context is already
// cancelled returns promptly with ctx.Err() instead of blocking on the lock.
func TestEnsureVenvWaiterHonoursCtxCancellation(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	counter := filepath.Join(t.TempDir(), "uv-invocations")
	uv := fakeUVDelayed(t, counter, "0.5")

	m := manifestFor(t, project, "jmespath==1.1.0\n")
	spec := VenvSpec{CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv}

	firstDone := make(chan struct{})
	go func() {
		if _, err := EnsureVenv(context.Background(), spec); err != nil {
			t.Errorf("first caller: %v", err)
		}
		close(firstDone)
	}()

	time.Sleep(50 * time.Millisecond)

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := EnsureVenv(cctx, spec)
	elapsed := time.Since(start)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter err = %v, want context.Canceled", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("waiter took %v, expected prompt return", elapsed)
	}

	<-firstDone

	v, err := EnsureVenv(context.Background(), spec)
	if err != nil {
		t.Fatalf("post-cancel ensure: %v", err)
	}
	if v.Created {
		t.Error("post-cancel ensure re-provisioned")
	}
}

// TestEnsureVenvRefusesSymlinkedLock: the lock file is opened without following
// symlinks, so a link planted at its path is refused and its target is never
// created or locked.
func TestEnsureVenvRefusesSymlinkedLock(t *testing.T) {
	cache := t.TempDir()
	project := t.TempDir()
	counter := filepath.Join(t.TempDir(), "uv-invocations")
	uv := fakeUV(t, counter)

	m := manifestFor(t, project, "jmespath==1.1.0\n")
	spec := VenvSpec{CacheRoot: cache, Source: "demo", Manifest: m, UVBinary: uv}

	target := filepath.Join(t.TempDir(), "victim")
	if err := os.Symlink(target, venvLockPath(spec)); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}
	if _, err := EnsureVenv(context.Background(), spec); err == nil {
		t.Fatal("EnsureVenv followed a symlinked lock file")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("symlink target was created (stat err = %v)", err)
	}
	if got := countUVLinesContaining(t, counter, "venv"); got != 0 {
		t.Errorf("uv ran %d times despite the refused lock", got)
	}
}
