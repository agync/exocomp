package deps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// galaxyBinaryName is the resolver the control plane invokes for project
// collections. ansible-galaxy ships with ansible-core in the image and in the
// per-source venv; EnsureCollections resolves it on PATH unless the spec pins
// one.
const galaxyBinaryName = "ansible-galaxy"

// galaxyReadyMarker sits at a collections dir's root once it is fully
// provisioned. Its presence (carrying the requirements hash) is what makes the
// cache hash-gated: an existing marker means reuse with no ansible-galaxy
// invocation, while a half-built dir from a crashed resolve has none and is
// rebuilt rather than trusted (the venv plane's readyMarker, mirrored).
const galaxyReadyMarker = ".exocomp-galaxy-ready"

// Galaxy cache layout. The resolver's HOME must never be a granted path: nono
// keeps its own protected state root under $HOME (the same rule jobDir in the
// executor documents) and refuses to launch when a grant would let the
// sandboxed process reach it. So the galaxy cache root is split into an
// ungranted HOME and granted children that cover exactly what ansible-galaxy
// writes.

// galaxyCacheHomeSubdir is the resolver's HOME directory under the galaxy
// cache root. It is created for the resolver but never granted.
const galaxyCacheHomeSubdir = "home"

// galaxyAnsibleSubdir is the ANSIBLE_HOME directory under the resolver's HOME,
// granted read+write: ansible-galaxy's GALAXY_CACHE_DIR and DEFAULT_LOCAL_TMP
// both default under it (ANSIBLE_HOME defaults to ~/.ansible), so this one
// directory covers every write the resolver makes — the collections half of
// the uv-cache grant, and the same ~/.ansible the ansible run driver grants.
const galaxyAnsibleSubdir = ".ansible"

// galaxyTmpSubdir is the resolver's TMPDIR under the galaxy cache root,
// granted read+write so a library that falls back to gettempdir() lands on a
// path the sandbox allows instead of nono's write-only /tmp.
const galaxyTmpSubdir = "tmp"

// GalaxyHome is the resolver's HOME under a galaxy cache root. It is created
// by the daemon and deliberately never granted.
func GalaxyHome(cacheRoot string) string {
	return filepath.Join(cacheRoot, galaxyCacheHomeSubdir)
}

// GalaxyAnsibleHome is the ANSIBLE_HOME directory under a galaxy cache root's
// HOME: the granted write path that carries galaxy's download and temp caches.
func GalaxyAnsibleHome(cacheRoot string) string {
	return filepath.Join(GalaxyHome(cacheRoot), galaxyAnsibleSubdir)
}

// GalaxyTmpDir is the resolver's TMPDIR under a galaxy cache root, granted
// read+write.
func GalaxyTmpDir(cacheRoot string) string {
	return filepath.Join(cacheRoot, galaxyTmpSubdir)
}

// GalaxyCacheGrants are the write paths a resolver needs under a galaxy cache
// root: ANSIBLE_HOME and TMPDIR. The resolver's HOME itself is never among
// them — a grant on HOME would cover nono's protected state root under it and
// be refused at launch.
func GalaxyCacheGrants(cacheRoot string) []string {
	return []string{GalaxyAnsibleHome(cacheRoot), GalaxyTmpDir(cacheRoot)}
}

// CollectionsSpec describes the collections to ensure for one source's galaxy
// manifest.
type CollectionsSpec struct {
	// CacheRoot is <data dir>/cache/collections; the collections land at
	// CacheRoot/<Source>/<requirements hash>.
	CacheRoot string
	// Source is the safe cache namespace for this project (a source name or a
	// slug of its root). It is a single path component.
	Source string
	// Manifest is the resolved galaxy manifest; EnsureCollections requires
	// Found.
	Manifest GalaxyManifest
	// GalaxyBinary overrides the ansible-galaxy executable. Empty resolves
	// ansible-galaxy on PATH.
	GalaxyBinary string
	// Offline passes ansible-galaxy's --offline, so resolution is served from
	// the cache alone with no network.
	Offline bool
	// Sandbox, when set, wraps every ansible-galaxy invocation in the job
	// sandbox tier so the resolver — a requirements file steers galaxy to
	// galaxy servers and installer code no more trusted than a manifest's
	// build backend — never runs in the daemon context. Nil runs galaxy
	// directly, which the hermetic unit tests and a host with no sandbox both
	// rely on.
	Sandbox SandboxLauncher
	// GalaxyCacheDir, when set, is the galaxy cache root: the resolver's HOME
	// and TMPDIR are redirected under it — HOME onto a child directory that is
	// deliberately not granted (nono keeps its own state under $HOME and
	// refuses a grant that reaches it), ANSIBLE_HOME and TMPDIR onto granted
	// children — so galaxy's download/temp cache lands on a granted,
	// backed-up path instead of the daemon's home, which the sandbox does not
	// grant. Empty leaves galaxy's defaults (the daemon's home), for a host
	// with no sandbox.
	GalaxyCacheDir string
	// KeepN is the newest-N retention window PruneCollections applies to this
	// source's other collections dirs after a successful provision (the dir
	// just built is protected outright, on top of naturally landing in this
	// window). Zero uses DefaultKeepVenvs.
	KeepN int
	// KeepSince bounds the retention window's other half: a dir built within
	// KeepSince of now is kept regardless of KeepN. Zero uses
	// DefaultKeepVenvsSince.
	KeepSince time.Duration
	// Logger receives a warning if the post-provision prune fails; the
	// failure never fails EnsureCollections itself (see pruneStaleCollections).
	// Nil uses slog.Default().
	Logger *slog.Logger
}

// Collections is a provisioned per-source ansible collections dir.
type Collections struct {
	// Dir is the collections root, CacheRoot/<Source>/<hash>.
	Dir string
	// Requirements is the absolute requirements file the install read.
	Requirements string
	// Created is true when ansible-galaxy was invoked on this call; false on
	// a cache hit.
	Created bool
}

// EnsureCollections returns the collections dir for spec.Manifest, provisioning
// it with ansible-galaxy only when the hash-gated marker is absent. On a cache
// hit it invokes no ansible-galaxy and touches no network. It is fail-closed:
// a provisioning failure removes the half-built dir and returns an error, so a
// run never proceeds against half-met collections (ADR 0015).
//
// A successful provision also prunes this source's other stale collections
// dirs (PruneCollections, keep-policy defaults DefaultKeepVenvs/
// DefaultKeepVenvsSince, overridable via spec), protecting the hash just built
// — otherwise the cache grows one directory per requirements hash forever.
// Pruning never fails this call; see pruneStaleCollections.
func EnsureCollections(ctx context.Context, spec CollectionsSpec) (Collections, error) {
	if !spec.Manifest.Found() {
		return Collections{}, errors.New("deps: EnsureCollections called for a source with no collections requirements")
	}
	if spec.CacheRoot == "" {
		return Collections{}, errors.New("deps: EnsureCollections needs a cache root")
	}
	if !safeComponent(spec.Source) {
		return Collections{}, fmt.Errorf("deps: source %q is not a safe cache component", spec.Source)
	}

	dir := filepath.Join(spec.CacheRoot, spec.Source, spec.Manifest.Hash)
	c := Collections{Dir: dir, Requirements: spec.Manifest.Path}

	if galaxyMarkerMatches(dir, spec.Manifest.Hash) {
		return c, nil
	}

	if err := provisionCollections(ctx, spec, dir); err != nil {
		// A partial dir would be reused as if complete on the next run; remove
		// it so the failure is not sticky.
		_ = os.RemoveAll(dir)
		return Collections{}, err
	}
	c.Created = true
	pruneStaleCollections(spec, spec.Manifest.Hash)
	return c, nil
}

// provisionCollections installs the requirements into dir, writing the ready
// marker only after ansible-galaxy exits clean.
func provisionCollections(ctx context.Context, spec CollectionsSpec, dir string) error {
	galaxy := spec.GalaxyBinary
	if galaxy == "" {
		resolved, err := exec.LookPath(galaxyBinaryName)
		if err != nil {
			return fmt.Errorf("deps: %w", err)
		}
		galaxy = resolved
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("deps: cache dir: %w", err)
	}
	// Same rationale as VenvSpec.UVCacheDir: under the sandbox tier a Landlock
	// write grant attaches only to a path that already exists, and galaxy's own
	// mkdir of its download/temp tree would need write on the ungranted parent
	// cache tree. Create the granted cache dirs here in the daemon so galaxy
	// fills their subtrees, along with its ungranted HOME (GalaxyHome) so the
	// resolver never has to create it.
	if spec.GalaxyCacheDir != "" {
		dirs := append([]string{GalaxyHome(spec.GalaxyCacheDir)}, GalaxyCacheGrants(spec.GalaxyCacheDir)...)
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return fmt.Errorf("deps: galaxy cache dir: %w", err)
			}
		}
	}
	// A leftover partial from an earlier failed attempt must not confuse galaxy.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("deps: clear stale collections: %w", err)
	}

	if err := runGalaxy(ctx, galaxy, spec, dir); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, galaxyReadyMarker), []byte(spec.Manifest.Hash+"\n"), 0o600); err != nil {
		return fmt.Errorf("deps: write ready marker: %w", err)
	}
	return nil
}

// runGalaxy invokes ansible-galaxy with the resolver environment. Correctness
// is the exit code (AGENTS.md invariant 3); galaxy's stderr rides in the
// returned error for the operator, never parsed for a verdict. When the spec
// carries a sandbox the argv is wrapped in the launcher first, so a
// requirements file steering galaxy to galaxy servers and installer code runs
// confined rather than with daemon privileges.
func runGalaxy(ctx context.Context, galaxy string, spec CollectionsSpec, dir string) error {
	argv := []string{galaxy, "collection", "install"}
	if spec.Offline {
		argv = append(argv, "--offline")
	}
	argv = append(argv, "-r", spec.Manifest.Path, "-p", dir)
	if spec.Sandbox != nil {
		wrapped, err := spec.Sandbox.Wrap(argv)
		if err != nil {
			return fmt.Errorf("deps: sandbox ansible-galaxy collection install: %w", err)
		}
		argv = wrapped
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// The resolver runs with its working directory inside the granted cache
	// tree: under the sandbox an ungranted cwd is refused outright, and the
	// per-source cache dir is a write grant the profile already carries.
	cmd.Dir = filepath.Dir(dir)
	cmd.Env = galaxyEnv(spec.GalaxyCacheDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("deps: ansible-galaxy collection install: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// galaxyEnv is the resolver environment: the daemon's environment with HOME
// redirected onto the galaxy cache root's ungranted home directory, and
// ANSIBLE_HOME and TMPDIR redirected onto the granted children under it. HOME
// stays ungranted because nono keeps its own state under $HOME and refuses a
// grant that reaches it; ANSIBLE_HOME and TMPDIR are the paths ansible-galaxy
// actually writes, and the profile grants exactly those. The redirects replace
// existing entries rather than appending: getenv returns the first match, so a
// late duplicate would be inert.
func galaxyEnv(cacheDir string) []string {
	env := os.Environ()
	if cacheDir == "" {
		return env
	}
	redirect := map[string]string{
		"HOME":         GalaxyHome(cacheDir),
		"ANSIBLE_HOME": GalaxyAnsibleHome(cacheDir),
		"TMPDIR":       GalaxyTmpDir(cacheDir),
	}
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if ok && redirect[name] != "" {
			continue
		}
		out = append(out, kv)
	}
	for _, name := range []string{"HOME", "ANSIBLE_HOME", "TMPDIR"} {
		out = append(out, name+"="+redirect[name])
	}
	return out
}

// galaxyMarkerMatches reports whether dir holds a ready marker for the given
// hash. A mismatching marker (a hash-collision path reused, or a truncated
// write) is treated as absent so the dir is rebuilt rather than trusted.
func galaxyMarkerMatches(dir, hash string) bool {
	body, err := os.ReadFile(filepath.Join(dir, galaxyReadyMarker))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(body)) == hash
}

// MarkCollectionsReady writes the ready marker a provisioned collections dir
// carries, so a later EnsureCollections reads it as a cache hit and never
// re-dials galaxy. It is the counterpart to provisionCollections' marker
// write, byte-identical (the hash plus a newline), exposed so `playbook
// vendor` can mark the dir it populated offline (ADR 0015): without it a
// vendored dir with no marker is rebuilt — and its contents removed — on the
// next run.
func MarkCollectionsReady(dir, hash string) error {
	return os.WriteFile(filepath.Join(dir, galaxyReadyMarker), []byte(hash+"\n"), 0o600)
}

// pruneStaleCollections runs PruneCollections for spec's source after a
// successful provision, protecting the hash just built — the collections half
// of pruneStaleVenvs. A prune error is logged, never returned, so a provision
// that already succeeded cannot be turned into a failure by cleanup.
func pruneStaleCollections(spec CollectionsSpec, protect string) {
	keepN := spec.KeepN
	if keepN <= 0 {
		keepN = DefaultKeepVenvs
	}
	since := spec.KeepSince
	if since <= 0 {
		since = DefaultKeepVenvsSince
	}
	log := spec.Logger
	if log == nil {
		log = slog.Default()
	}
	if _, err := PruneCollections(spec.CacheRoot, spec.Source, keepN, time.Now().Add(-since), protect); err != nil {
		log.Warn("deps: prune stale collections failed", "source", spec.Source, "error", err)
	}
}

// RemoveProjectCaches removes every dependency cache plane for the project
// rooted at projectRoot: cache/python/<SourceKey(root)> and
// cache/collections/<SourceKey(root)> under dataDir. The source lifecycle's GC
// calls it after a source is deleted, and only when no other registered source
// resolves to the same key. Absence is tolerated (a source that never synced,
// or already removed).
func RemoveProjectCaches(dataDir, projectRoot string) error {
	key := SourceKey(projectRoot)
	for _, plane := range []string{"python", "collections"} {
		dir := filepath.Join(dataDir, "cache", plane, key)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("deps: remove cache/%s/%s: %w", plane, key, err)
		}
	}
	return nil
}
