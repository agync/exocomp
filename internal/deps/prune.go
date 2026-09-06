package deps

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultKeepVenvs and DefaultKeepVenvsSince are the keep policy PruneVenvs
// and PruneCollections apply by default, and what EnsureVenv/EnsureCollections
// use after every successful provision. Without a reaper,
// cache/{python,collections}/<source>/<hash> grows one directory per manifest
// hash forever — unlike restic (forget --prune) and tofu state
// (store.PruneTFStates), nothing bounded these caches before.
const (
	DefaultKeepVenvs      = 3
	DefaultKeepVenvsSince = 30 * 24 * time.Hour
)

// PruneVenvs deletes stale <manifest-hash> venv directories under
// cacheRoot/source, applying store.PruneTFStates's keep-both policy: the
// newest keepN venvs by recency are always retained, and so is every venv
// whose recency is at or after keepSince — a venv is pruned only when it
// fails BOTH checks. protect, when non-empty, names a hash that is excluded
// from pruning outright regardless of keepN or age: belt-and-braces for the
// venv EnsureVenv just (re)built or is about to reuse, beyond relying on it
// naturally landing in the newest-keepN window. keepN below 1 is treated as
// 1, mirroring PruneTFStates. It returns the number of directories removed;
// a removal failure is collected but does not stop the rest of the sweep.
//
// Recency signal: the venv's ready marker mtime when present — the marker is
// written once, at the end of a successful provision (see provision in
// venv.go), and never touched again on a cache hit, so its mtime is when the
// venv was last built, not last used. A venv with no marker (half-built, or
// predating this prune) falls back to the venv directory's own mtime.
func PruneVenvs(cacheRoot, source string, keepN int, keepSince time.Time, protect string) (int, error) {
	return pruneHashDirs(filepath.Join(cacheRoot, source), readyMarker, keepN, keepSince, protect, "venv")
}

// pruneHashDirs is the keep-N/since sweep both planes share: newest first by
// recency, retain index < keepN or recency >= keepSince, delete the rest,
// honoring protect. markerName selects the plane's ready marker for the
// recency signal; a directory with no marker falls back to its own mtime. what
// names the plane in errors. A removal failure is collected but does not stop
// the rest of the sweep.
func pruneHashDirs(sourceDir, markerName string, keepN int, keepSince time.Time, protect string, what string) (int, error) {
	if keepN < 1 {
		keepN = 1
	}
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("deps: prune %ss: %w", what, err)
	}

	type hashDir struct {
		hash   string
		dir    string
		recent time.Time
	}
	var live []hashDir
	for _, e := range entries {
		if !e.IsDir() || e.Name() == protect {
			continue
		}
		dir := filepath.Join(sourceDir, e.Name())
		live = append(live, hashDir{hash: e.Name(), dir: dir, recent: hashDirRecency(dir, markerName)})
	}

	// Newest first, so index < keepN is exactly the retained window.
	sort.Slice(live, func(i, j int) bool { return live[i].recent.After(live[j].recent) })

	var pruned int
	var firstErr error
	for i, v := range live {
		if i < keepN || !v.recent.Before(keepSince) {
			continue
		}
		if err := os.RemoveAll(v.dir); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("deps: prune %s %s: %w", what, v.hash, err)
			}
			continue
		}
		pruned++
	}
	return pruned, firstErr
}

// hashDirRecency is the best-available recency signal for a hash-keyed cache
// dir: the plane's ready marker's mtime, falling back to the directory's own
// mtime. See pruneHashDirs.
func hashDirRecency(dir, markerName string) time.Time {
	if fi, err := os.Stat(filepath.Join(dir, markerName)); err == nil {
		return fi.ModTime()
	}
	if fi, err := os.Stat(dir); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// pruneStaleVenvs runs PruneVenvs for spec's source after a successful
// provision, protecting the hash just built. Pruning is best-effort by
// design (BUILD unit 1): a provision that already succeeded must not be
// turned into a failure by a cleanup step downstream of it, and removing a
// stale venv is always safe to skip — it is simply reconsidered on the next
// provision — so a prune error is logged, never returned.
func pruneStaleVenvs(spec VenvSpec, protect string) {
	keepN := spec.KeepVenvs
	if keepN <= 0 {
		keepN = DefaultKeepVenvs
	}
	since := spec.KeepVenvsSince
	if since <= 0 {
		since = DefaultKeepVenvsSince
	}
	log := spec.Logger
	if log == nil {
		log = slog.Default()
	}
	if _, err := PruneVenvs(spec.CacheRoot, spec.Source, keepN, time.Now().Add(-since), protect); err != nil {
		log.Warn("deps: prune stale venvs failed", "source", spec.Source, "error", err)
	}
}
