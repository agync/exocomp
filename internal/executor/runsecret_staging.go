package executor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
)

// StagingBackend names where a run-secret that must reach a tool as a file is
// staged.
type StagingBackend string

const (
	// StagingAuto uses the tmpfs root when it is usable (present, actually
	// tmpfs, adequate free space) and falls back to the disk root otherwise —
	// loudly, never silently (ADR 0005's posture applied to this path). It is
	// the default.
	StagingAuto StagingBackend = "auto"
	// StagingTmpfs forces the tmpfs root and fails closed if it is unusable:
	// the strict "never a secret on disk" posture.
	StagingTmpfs StagingBackend = "tmpfs"
	// StagingDisk forces the disk root explicitly.
	StagingDisk StagingBackend = "disk"
)

// ParseStagingBackend reads a backend name. Empty means StagingAuto.
func ParseStagingBackend(s string) (StagingBackend, error) {
	switch StagingBackend(s) {
	case "":
		return StagingAuto, nil
	case StagingAuto, StagingTmpfs, StagingDisk:
		return StagingBackend(s), nil
	default:
		return "", fmt.Errorf("executor: unknown secret staging backend %q (want %s, %s, or %s)",
			s, StagingAuto, StagingTmpfs, StagingDisk)
	}
}

// stagingSessionPrefix names every session directory this package creates
// under a staging root: "exocomp.<pid>.<rand>". SweepOrphanRunSecretFiles
// parses it back apart to test the owning pid's liveness, and it is what lets
// the sweep tell this process's own instances apart from a concurrent sibling
// daemon's and from anything else that happens to live in the same root
// (ansible-core's own POSIX semaphores under /dev/shm, above all).
const stagingSessionPrefix = "exocomp."

// StagingConfig selects and resolves the run-secret file staging backend.
type StagingConfig struct {
	// Backend is the operator's choice. Empty means StagingAuto.
	Backend StagingBackend
	// DiskRoot is the disk staging root ("<data-dir>/run/"), required for
	// StagingDisk and for StagingAuto's fallback. StagingTmpfs never reads it.
	DiskRoot string
	// TmpfsRootOverride replaces TmpfsRoot for the usability probe. Empty uses
	// TmpfsRoot. It exists so a test can point the probe at a path that is
	// deliberately not tmpfs, without touching the real /dev/shm.
	TmpfsRootOverride string
	// MinTmpfsFree replaces minTmpfsFreeBytes for the usability probe. Zero
	// uses the default floor.
	MinTmpfsFree int64
	// Warn receives the loud, one-line notice a StagingAuto fallback to disk
	// writes — ADR 0005's never-silently-downgrade posture. Nil discards it
	// (still returned as ResolvedStaging.FellBack for a caller to report
	// through its own channel).
	Warn io.Writer
}

// Staging is a resolved run-secret file staging backend together with this
// process's own session directory: <root>/exocomp.<pid>.<rand>/, created
// lazily on the first file actually staged and removed in full by Close. Every
// file StageFiles writes lives inside it, so a single RemoveAll — the
// graceful-shutdown path in cmd/exocomp's runServe — reclaims everything this
// instance ever staged, and the startup sweep can reason about ownership at
// the directory level instead of chasing individual files.
type Staging struct {
	// Backend is what was actually resolved: StagingTmpfs or StagingDisk,
	// never StagingAuto — auto is a request, not an outcome.
	Backend StagingBackend
	// Root is the staging root this instance stages into: TmpfsRoot (or its
	// override) for StagingTmpfs, DiskRoot for StagingDisk.
	Root string
	// FellBack reports whether StagingAuto asked for tmpfs and got disk
	// instead because tmpfs was unusable.
	FellBack bool
	// TmpfsUsable and TmpfsReason are the tmpfs probe's own verdict,
	// independent of which backend was actually resolved — doctor reads both
	// so an operator running the disk backend on purpose still sees whether
	// tmpfs would work if they switched.
	TmpfsUsable bool
	TmpfsReason string

	sessionDir atomic.Pointer[string]
}

// ResolveStaging decides the effective backend from cfg, probing tmpfs
// usability exactly once. It never mutates process-global state; a caller
// that wants every subsequent StageRunSecretFiles call in this process to use
// the result calls ConfigureRunSecretStaging instead.
func ResolveStaging(cfg StagingConfig) (*Staging, error) {
	tmpfsRoot := cfg.TmpfsRootOverride
	if tmpfsRoot == "" {
		tmpfsRoot = TmpfsRoot
	}
	usable, reason := tmpfsUsable(tmpfsRoot, cfg.MinTmpfsFree)

	backend := cfg.Backend
	if backend == "" {
		backend = StagingAuto
	}

	switch backend {
	case StagingTmpfs:
		if !usable {
			return nil, fmt.Errorf("executor: secret staging: tmpfs backend requested but %s is unusable: %s",
				tmpfsRoot, reason)
		}
		return &Staging{Backend: StagingTmpfs, Root: tmpfsRoot, TmpfsUsable: true}, nil

	case StagingDisk:
		if cfg.DiskRoot == "" {
			return nil, errors.New("executor: secret staging: disk backend requested but no disk root is configured")
		}
		return &Staging{
			Backend: StagingDisk, Root: cfg.DiskRoot,
			TmpfsUsable: usable, TmpfsReason: reason,
		}, nil

	case StagingAuto:
		if usable {
			return &Staging{Backend: StagingTmpfs, Root: tmpfsRoot, TmpfsUsable: true}, nil
		}
		if cfg.DiskRoot == "" {
			return nil, fmt.Errorf(
				"executor: secret staging: tmpfs is unusable (%s) and no disk fallback root is configured", reason)
		}
		warn := cfg.Warn
		if warn == nil {
			warn = io.Discard
		}
		_, _ = fmt.Fprintf(warn,
			"WARNING: run-secret file staging falling back to disk at %s: tmpfs (%s) is unusable: %s; "+
				"a captured secret's file will touch durable storage (mlock best-effort; see `exocomp doctor`)\n",
			cfg.DiskRoot, tmpfsRoot, reason)
		return &Staging{
			Backend: StagingDisk, Root: cfg.DiskRoot, FellBack: true,
			TmpfsUsable: false, TmpfsReason: reason,
		}, nil

	default:
		return nil, fmt.Errorf("executor: secret staging: unknown backend %q", backend)
	}
}

// ensureSessionDir returns this instance's session directory, creating it
// 0700 on first use. Concurrent jobs staging their first file at the same
// moment race harmlessly: os.Mkdir tolerates EEXIST (treated as success, not
// an error) so whichever goroutine loses the mkdir race simply reuses the
// directory the winner just created, and only one of them actually wins the
// atomic.Pointer swap that publishes the winning path. Treating EEXIST as
// success is intentional here, not a masked bug: the directory name's random
// suffix (randomSessionDirName, crypto/rand) makes a collision with anything
// other than this same process's own concurrent call practically impossible,
// so EEXIST in practice only ever means "this instance already created it."
func (s *Staging) ensureSessionDir() (string, error) {
	if p := s.sessionDir.Load(); p != nil {
		return *p, nil
	}
	name, err := randomSessionDirName()
	if err != nil {
		return "", fmt.Errorf("random session dir name: %w", err)
	}
	dir := filepath.Join(s.Root, name)
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create staging session dir %s: %w", dir, err)
	}
	s.sessionDir.CompareAndSwap(nil, &dir)
	return *s.sessionDir.Load(), nil
}

// SessionDir returns this instance's session directory, or "" if no file has
// been staged yet (the directory is created lazily).
func (s *Staging) SessionDir() string {
	if p := s.sessionDir.Load(); p != nil {
		return *p
	}
	return ""
}

// Close removes this instance's whole session directory, if one was ever
// created — the graceful-shutdown layer of the three-layer cleanup (per-job
// unlink on Close, this RemoveAll on a clean stop, the startup liveness sweep
// for everything else). Safe to call when nothing was ever staged, and safe to
// call more than once.
func (s *Staging) Close() error {
	p := s.sessionDir.Swap(nil)
	if p == nil {
		return nil
	}
	if err := os.RemoveAll(*p); err != nil {
		return fmt.Errorf("executor: remove staging session dir %s: %w", *p, err)
	}
	return nil
}

// randomSessionDirName returns "exocomp.<pid>.<rand>".
func randomSessionDirName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%d.%s", stagingSessionPrefix, os.Getpid(), hex.EncodeToString(b)), nil
}

// activeStaging is the process-wide backend StageRunSecretFiles and
// SweepOrphanRunSecretFiles' free-function callers (internal/executor's own
// tofu.go and sandbox_drivers.go) use, resolved once and read many times —
// the same "decide once per process, everyone reads the same answer" shape
// cmd/exocomp's own sandbox tier decision (cmd/exocomp/sandbox.go) uses, kept
// here instead because the resolution (auto/tmpfs/disk, the tmpfs probe) is
// this package's own concern.
type stagingResult struct {
	st  *Staging
	err error
}

var activeStaging atomic.Pointer[stagingResult]

// ConfigureRunSecretStaging resolves cfg and makes it the backend every
// subsequent StageRunSecretFiles call in this process uses. It is meant to run
// once, at daemon startup, before the queue's workers start — cmd/exocomp's
// runServe calls it right after the store (and so the persisted setting) is
// available. A caller that never calls it gets the lazy default: StagingAuto
// with no disk root, i.e. tmpfs-only — the same never-on-disk guarantee this
// path always had, so a bare CLI path or a test that stages a file without
// configuring anything keeps working exactly as before.
func ConfigureRunSecretStaging(cfg StagingConfig) (*Staging, error) {
	st, err := ResolveStaging(cfg)
	activeStaging.Store(&stagingResult{st: st, err: err})
	return st, err
}

// currentRunSecretStaging returns the active backend, resolving the lazy
// default on first use if ConfigureRunSecretStaging was never called.
func currentRunSecretStaging() (*Staging, error) {
	if r := activeStaging.Load(); r != nil {
		return r.st, r.err
	}
	st, err := ResolveStaging(StagingConfig{Backend: StagingAuto})
	activeStaging.CompareAndSwap(nil, &stagingResult{st: st, err: err})
	r := activeStaging.Load()
	return r.st, r.err
}

// CloseRunSecretStaging removes the active backend's whole session directory,
// if one was ever created. It is the daemon graceful-shutdown call — wired as
// a defer in cmd/exocomp's runServe — and a no-op when nothing ever staged a
// file in this process.
func CloseRunSecretStaging() error {
	r := activeStaging.Load()
	if r == nil || r.st == nil {
		return nil
	}
	return r.st.Close()
}
