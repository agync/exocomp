package executor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"exocomp.dev/exocomp/internal/secrets"
)

// StagedRunSecretFile is one run-secret value staged as a 0600 file inside the
// active backend's session directory: the path a sandboxed child needs a
// --read-file grant for, and whether the bytes were pinned out of swap.
type StagedRunSecretFile struct {
	// Env is the run-secret name the file was staged for.
	Env string
	// Path is the file's location. It is what the env is rewritten to, and
	// what the caller must add to its sandbox profile's ReadFiles.
	Path string
	// Backend is the backend the file actually landed on. A caller compares
	// it against StagingTmpfs to decide whether to surface the disk residual
	// on this particular run (tofu.go and sandbox_drivers.go do, as a
	// LevelSystem job event) — the auto-fallback warning at daemon startup is
	// a one-time notice, but a job that actually carries a secret onto disk
	// deserves its own record in that job's own log.
	Backend StagingBackend
	// Lock reports whether the staged bytes were mlocked. Best-effort:
	// Lock.Locked == false is a documented residual, never a staging
	// failure — there is no rootless `noswap` tmpfs option, so mlock is the
	// only swap lever available at all, on either backend.
	Lock secrets.MemoryLock

	mapped []byte // the mmap'd region Lock pins; released by Close.
}

// Close unlocks and unmaps the staged file's bytes, then unlinks it. The
// caller runs it once per staged file after the subprocess that read it has
// exited (Wait returned) — POSIX unlink-while-open holds under nono, so
// deferring it immediately after staging, before the subprocess runs, is
// correct and is the same shape as jobDir's own deferred cleanup. This is
// layer one of the three-layer cleanup (per-job unlink here; the whole session
// directory on a graceful shutdown, Staging.Close; a liveness-checked sweep of
// leftover session directories at startup, SweepOrphanRunSecretFiles).
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
// 0600 file inside the active backend's session directory (O_CREATE|O_EXCL, a
// random name — never derived from the env name, so concurrent jobs staging
// the same env never collide or overwrite one another) and rewrites that env
// from its value to the file's path, so a tool that authenticates from a
// KUBECONFIG file (kubectl/helm, an exec-auth provider) reads the credential
// from a path while the value itself only ever exists there.
//
// The backend in effect is whatever ConfigureRunSecretStaging last resolved
// for this process, or the lazy tmpfs-only default if that was never called
// (see currentRunSecretStaging).
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
	st, err := currentRunSecretStaging()
	if err != nil {
		return nil, nil, fmt.Errorf("executor: stage run-secrets: %w", err)
	}
	return st.StageFiles(rs, fileEnv)
}

// StageFiles is StageRunSecretFiles against this specific, already-resolved
// backend instance rather than the process-wide active one — what a test
// exercising backend selection calls directly (ResolveStaging then
// StageFiles), and what the free function above delegates to for the
// production path.
func (s *Staging) StageFiles(rs RunSecrets, fileEnv []string) (RunSecrets, []StagedRunSecretFile, error) {
	if len(fileEnv) == 0 {
		return rs, nil, nil
	}

	out := make(RunSecrets, len(rs))
	for k, v := range rs {
		out[k] = v
	}

	dir, err := s.ensureSessionDir()
	if err != nil {
		return nil, nil, fmt.Errorf("executor: stage run-secrets: %w", err)
	}

	var staged []StagedRunSecretFile
	for _, name := range fileEnv {
		v, ok := rs[name]
		if !ok {
			continue
		}
		sf, err := stageOneRunSecretFile(dir, s.Backend, name, v)
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

// noteDiskStagingResidual emits one LevelSystem job event when any file in
// staged landed on the disk backend rather than tmpfs. The auto-fallback
// warning ConfigureRunSecretStaging writes at daemon startup is a one-time
// notice on the daemon's own log; a job that actually carries a secret's
// bytes onto durable storage during its own run deserves a record in that
// run's own event log too, next to "tofu plan" and every other LevelSystem
// line an operator already reads there.
func noteDiskStagingResidual(sink EventSink, staged []StagedRunSecretFile) {
	for _, sf := range staged {
		if sf.Backend != StagingDisk {
			continue
		}
		sink.Emit(Event{
			Ts:    time.Now().UTC(),
			Level: LevelSystem,
			Msg: fmt.Sprintf(
				"run-secret %s staged to disk, not tmpfs: 0600 file, best-effort mlock, "+
					"unlinked at run end (docs/operations/serve.md)", sf.Env),
		})
		return // one notice per run is enough.
	}
}

// stageOneRunSecretFile writes value to a fresh 0600 file under dir and
// best-effort mlocks its mapped bytes. dir is the active backend's session
// directory, on tmpfs or disk alike — the mlock attempt is backend-agnostic,
// since mmap'd pages are pinnable regardless of the filesystem backing them,
// and pinning them is exactly the swap defence the disk backend's residual
// needs (there is no rootless `noswap` tmpfs mount option, so this is the only
// swap lever available on either backend).
func stageOneRunSecretFile(dir string, backend StagingBackend, env, value string) (StagedRunSecretFile, error) {
	path, f, err := createStagingFile(dir)
	if err != nil {
		return StagedRunSecretFile{}, err
	}
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(value); err != nil {
		_ = os.Remove(path)
		return StagedRunSecretFile{}, fmt.Errorf("write: %w", err)
	}

	staged := StagedRunSecretFile{Env: env, Path: path, Backend: backend}
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

// createStagingFile creates a fresh, exclusively-owned 0600 file under dir
// with a random name, and returns it open for read+write. O_EXCL is the
// collision guard: a name reused across two concurrent jobs is rejected
// rather than silently shared.
func createStagingFile(dir string) (string, *os.File, error) {
	name, err := randomHex(16)
	if err != nil {
		return "", nil, fmt.Errorf("random name: %w", err)
	}
	path := filepath.Join(dir, "exocomp-secret-"+name)
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

// SweepOrphanRunSecretFiles removes every this-package-owned session
// directory under each of roots whose owning pid is no longer alive. It is
// the third cleanup layer, run once at daemon startup (cmd/exocomp/serve.go,
// before the store opens): the normal path already unlinks a staged file
// after its subprocess exits, and a graceful stop removes the whole session
// directory (Staging.Close), but a daemon that survives a broken cleanup path
// (a panic between staging and Close, a kill -9 losing both) would otherwise
// leave a stale bearer sitting on roots indefinitely.
//
// Liveness, not blind prefix matching, is what makes this safe to run against
// a root a concurrent sibling exocomp instance might also be staging into: a
// session directory whose pid is still alive is left alone no matter how old
// it looks, so this can never delete another live instance's in-flight
// secrets — only ownerless directories a dead process's own crash left behind.
// Passing no roots defaults to []string{TmpfsRoot}, matching this function's
// pre-backend-abstraction behavior for a caller that only ever used tmpfs.
//
// It reports how many session directories it removed and the first error
// encountered, if any; a root that does not exist (a non-Linux dev box's
// missing /dev/shm, or a disk root nothing has ever staged into) is not an
// error, and anything under a root that this package did not name — ansible-
// core's own POSIX semaphores under /dev/shm, above all — is left untouched.
func SweepOrphanRunSecretFiles(roots ...string) (int, error) {
	if len(roots) == 0 {
		roots = []string{TmpfsRoot}
	}

	var removed int
	var firstErr error
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("executor: sweep %s: %w", root, err)
			}
			continue
		}

		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			pid, ok := parseStagingSessionDirName(e.Name())
			if !ok {
				continue
			}
			if pidAlive(pid) {
				continue
			}
			path := filepath.Join(root, e.Name())
			if err := os.RemoveAll(path); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("executor: sweep remove %s: %w", path, err)
				}
				continue
			}
			removed++
		}
	}
	return removed, firstErr
}

// parseStagingSessionDirName extracts the owning pid from a directory basename
// this package created ("exocomp.<pid>.<rand>"), reporting false for anything
// else it might find sharing the same root.
func parseStagingSessionDirName(name string) (pid int, ok bool) {
	rest, ok := strings.CutPrefix(name, stagingSessionPrefix)
	if !ok {
		return 0, false
	}
	pidStr, _, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(pidStr)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
