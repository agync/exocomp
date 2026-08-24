package executor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"exocomp.dev/exocomp/internal/secrets"
)

// ShmDir is where a run-secret value that must reach a tool as a file is
// staged: /dev/shm, the RAM-backed tmpfs present unconditionally in every
// rootless podman container. Unlike a `Tmpfs=` quadlet mount it needs no
// mount, no quadlet entry, and no capability, and it is what lets file
// staging work under the default local-sandboxed tier — a nono `--read-file`
// grant on the staged path is all a sandboxed child needs (see ReadFiles on
// SandboxOptions).
const ShmDir = "/dev/shm"

// shmFilePrefix names every file this package stages under ShmDir, so
// SweepOrphanRunSecretFiles can find and remove leftovers by name without
// touching anything else that lives there — ansible-core's own POSIX
// semaphores above all, which share the directory under the sandboxed tier.
const shmFilePrefix = "exocomp-secret-"

// StagedRunSecretFile is one run-secret value staged as a 0600 file under
// ShmDir: the path a sandboxed child needs a --read-file grant for, and
// whether the bytes were pinned out of swap.
type StagedRunSecretFile struct {
	// Env is the run-secret name the file was staged for.
	Env string
	// Path is the file's location under ShmDir. It is what the env is
	// rewritten to, and what the caller must add to its sandbox profile's
	// ReadFiles.
	Path string
	// Lock reports whether the staged bytes were mlocked. Best-effort:
	// Lock.Locked == false is a documented residual, never a staging
	// failure — there is no rootless `noswap` tmpfs option, so mlock is the
	// only swap lever available at all.
	Lock secrets.MemoryLock

	mapped []byte // the mmap'd region Lock pins; released by Close.
}

// Close unlocks and unmaps the staged file's bytes, then unlinks it. The
// caller runs it once per staged file after the subprocess that read it has
// exited (Wait returned) — POSIX unlink-while-open holds under nono, so
// deferring it immediately after staging, before the subprocess runs, is
// correct and is the same shape as jobDir's own deferred cleanup.
func (s *StagedRunSecretFile) Close() error {
	var errs []error
	if s.mapped != nil {
		if err := unix.Munlock(s.mapped); err != nil {
			errs = append(errs, fmt.Errorf("munlock %s: %w", s.Path, err))
		}
		if err := unix.Munmap(s.mapped); err != nil {
			errs = append(errs, fmt.Errorf("munmap %s: %w", s.Path, err))
		}
		s.mapped = nil
	}
	if s.Path != "" {
		if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", s.Path, err))
		}
	}
	return errors.Join(errs...)
}

// StageRunSecretFiles materializes each named run-secret value from rs into a
// 0600 file under ShmDir (O_CREATE|O_EXCL, a random name — never derived from
// the env name, so concurrent jobs staging the same env never collide or
// overwrite one another) and rewrites that env from its value to the file's
// path, so a tool that authenticates from a KUBECONFIG file (kubectl/helm, an
// exec-auth provider) reads the credential from a path while the value itself
// only ever exists under ShmDir — RAM, never durable disk.
//
// It returns a rewritten copy of rs (the input is left untouched, so a caller
// can fall back on it) and the staged files. The caller MUST:
//   - add each staged file's Path to its sandbox profile's ReadFiles, or a
//     sandboxed child cannot open it (nono default-denies everything ungranted);
//   - Close every staged file once the subprocess that used it has exited.
//
// A named env not present in rs is skipped — an empty capture binds nothing,
// the same empty-tolerance the ab:<workflow> resolution takes. On error,
// every file staged so far in this call is already cleaned up before
// returning, so a caller need not track partial results.
func StageRunSecretFiles(rs RunSecrets, fileEnv []string) (RunSecrets, []StagedRunSecretFile, error) {
	if len(fileEnv) == 0 {
		return rs, nil, nil
	}

	out := make(RunSecrets, len(rs))
	for k, v := range rs {
		out[k] = v
	}

	var staged []StagedRunSecretFile
	for _, name := range fileEnv {
		v, ok := rs[name]
		if !ok {
			continue
		}
		sf, err := stageOneRunSecretFile(name, v)
		if err != nil {
			for i := range staged {
				_ = staged[i].Close()
			}
			return nil, nil, fmt.Errorf("executor: stage run-secret %s: %w", name, err)
		}
		out[name] = sf.Path
		staged = append(staged, sf)
	}
	return out, staged, nil
}

// stageOneRunSecretFile writes value to a fresh 0600 file under ShmDir and
// best-effort mlocks its mapped bytes.
func stageOneRunSecretFile(env, value string) (StagedRunSecretFile, error) {
	path, f, err := createShmFile()
	if err != nil {
		return StagedRunSecretFile{}, err
	}
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(value); err != nil {
		_ = os.Remove(path)
		return StagedRunSecretFile{}, fmt.Errorf("write: %w", err)
	}

	staged := StagedRunSecretFile{Env: env, Path: path}
	if len(value) == 0 {
		// Nothing to map or lock; an empty value is not a security-relevant
		// residual.
		staged.Lock = secrets.MemoryLock{Locked: true}
		return staged, nil
	}

	mapped, err := unix.Mmap(int(f.Fd()), 0, len(value), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		// mmap failing is not a staging failure: the file is written and
		// readable either way, just unlocked. Reported like any other
		// mlock residual.
		staged.Lock = secrets.MemoryLock{Locked: false, Err: fmt.Errorf("mmap: %w", err)}
		return staged, nil
	}

	staged.Lock = secrets.LockBytes(mapped)
	if staged.Lock.Locked {
		staged.mapped = mapped // kept alive so the lock holds; released by Close.
	} else {
		_ = unix.Munmap(mapped)
	}
	return staged, nil
}

// createShmFile creates a fresh, exclusively-owned 0600 file under ShmDir
// with a random name, and returns it open for read+write. O_EXCL is the
// collision guard: a name reused across two concurrent jobs is rejected
// rather than silently shared.
func createShmFile() (string, *os.File, error) {
	name, err := randomHex(16)
	if err != nil {
		return "", nil, fmt.Errorf("random name: %w", err)
	}
	path := filepath.Join(ShmDir, shmFilePrefix+name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, fmt.Errorf("create %s: %w", path, err)
	}
	return path, f, nil
}

// randomHex returns n random bytes hex-encoded, for a staged file's basename.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SweepOrphanRunSecretFiles removes every exocomp-secret-* file under ShmDir.
// It is defense-in-depth run once at daemon startup: the normal path already
// unlinks a staged file after its subprocess exits, but a daemon that
// survives a broken cleanup path (a panic between staging and Close, a kill
// -9 losing the deferred unlink) would otherwise leave a stale bearer sitting
// in /dev/shm indefinitely — container `Restart=always` clears /dev/shm on
// most crashes, but the sweep covers the case where it does not restart the
// container (a bare-metal or non-container deployment of the same binary).
//
// It reports how many files it removed and the first error encountered, if
// any; a missing ShmDir (a non-Linux dev box) is not an error; other files
// under ShmDir are left alone.
func SweepOrphanRunSecretFiles() (int, error) {
	entries, err := os.ReadDir(ShmDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("executor: sweep %s: %w", ShmDir, err)
	}

	var removed int
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), shmFilePrefix) {
			continue
		}
		path := filepath.Join(ShmDir, e.Name())
		if err := os.Remove(path); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("executor: sweep remove %s: %w", path, err)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}
