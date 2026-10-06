package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"exocomp.dev/exocomp/internal/deps"
	"exocomp.dev/exocomp/internal/executor"
)

// pythonCacheDir is the venv cache root under the data dir. It sits beside
// cache/collections so both dependency planes live under one backed-up,
// offline-restorable tree (ADR 0011).
const pythonCacheDir = "cache/python"

// uvCacheSubdir is uv's own download cache, under the venv cache root so it is
// backed up with the venvs it seeds. It is a granted write path under the
// resolver sandbox; uv's default cache lives under the daemon home, which the
// sandbox does not grant.
const uvCacheSubdir = "uv-cache"

// galaxyTool is the ansible-galaxy binary a collections resolve invokes: on
// PATH, or a per-source venv's sibling when the run resolved one.
const galaxyTool = "ansible-galaxy"

// galaxyCacheDir is galaxy's download/temp cache root under the data dir, a
// sibling of the collections cache root rather than a child of it. It is a
// granted write path under the resolver sandbox; galaxy's default cache lives
// under the daemon home, which the sandbox does not grant. Keeping it out of
// cache/collections stops it being mistaken for a per-source collections dir,
// and the resolver's HOME inside it stays ungranted — nono refuses a grant
// that would reach its own state root under a granted HOME.
const galaxyCacheDir = "cache/galaxy"

// resolveProjectVenv resolves the per-source uv venv a playbook run executes
// inside (ADR 0011). It discovers the project's Python manifest from the
// playbook's directory; a source that declares none returns a zero Venv, so the
// run stays on the shared image toolchain — which is also what keeps DR-path
// runbooks (seeded standalone, carrying no manifest) on the image, never a
// project venv. A declared manifest is resolved into cache/python/<source>/<hash>,
// hash-gated so an unchanged manifest invokes no uv.
//
// It is fail-closed: a manifest that will not resolve returns an error, so a run
// never proceeds against half-met dependencies.
func resolveProjectVenv(ctx context.Context, dataDir, playbook string) (deps.Venv, error) {
	if playbook == "" {
		return deps.Venv{}, nil
	}
	start := filepath.Dir(playbook)
	manifest, err := deps.DiscoverPython(start)
	if err != nil {
		return deps.Venv{}, fmt.Errorf("deps: discover manifest for %s: %w", playbook, err)
	}
	if !manifest.HasDeps() {
		return deps.Venv{}, nil
	}
	cacheRoot := filepath.Join(dataDir, pythonCacheDir)
	source := deps.SourceKey(manifest.Root)
	spec := deps.VenvSpec{
		CacheRoot: cacheRoot,
		Source:    source,
		Manifest:  manifest,
	}
	// The resolver runs uv, and uv runs a manifest's build backend — code
	// derived from an untrusted source. Under the sandbox tier it is confined to
	// its venv cache and the manifest, denied the data dir and the keys; the
	// daemon-context resolve stays only where no sandbox can enforce it.
	if sandboxed(ctx) {
		uvCache := filepath.Join(cacheRoot, uvCacheSubdir)
		read := executor.ToolGrants(deps.UVBinary, "python3")
		read = append(read, manifest.Root)
		spec.UVCacheDir = uvCache
		spec.Sandbox = deps.NewResolverSandbox(deps.ResolverSandboxOptions{
			DataDir:    dataDir,
			VenvDir:    filepath.Join(cacheRoot, source),
			ReadPaths:  read,
			WritePaths: []string{uvCache},
		})
	}
	return deps.EnsureVenv(ctx, spec)
}

// resolveSourceCollections resolves the per-source ansible collections a
// playbook run executes with (ADR 0015), mirroring resolveProjectVenv. It
// discovers the project's galaxy manifest from the playbook's directory; a
// source that declares none returns a zero Collections, so the run falls back
// to the shared cache/collections dir — which is also what keeps DR-path
// runbooks (seeded standalone, carrying no manifest) on the shared fallback. A
// declared manifest is resolved into cache/collections/<source>/<hash>,
// hash-gated so an unchanged requirements file invokes no ansible-galaxy.
//
// It is fail-closed: a requirements file that will not resolve returns an
// error, so a run never proceeds against half-met collections.
func resolveSourceCollections(ctx context.Context, dataDir, playbook string) (deps.Collections, error) {
	if playbook == "" {
		return deps.Collections{}, nil
	}
	start := filepath.Dir(playbook)
	manifest, err := deps.DiscoverGalaxy(start)
	if err != nil {
		return deps.Collections{}, fmt.Errorf("deps: discover galaxy manifest for %s: %w", playbook, err)
	}
	if !manifest.Found() {
		return deps.Collections{}, nil
	}
	// Resolve the venv first: its ansible-galaxy sibling, when present, is this
	// source's collections resolver — the way its ansible-playbook is the run's
	// binary (ADR 0015). An absent venv leaves galaxy to PATH.
	venv, err := resolveProjectVenv(ctx, dataDir, playbook)
	if err != nil {
		return deps.Collections{}, err
	}
	cacheRoot := filepath.Join(dataDir, collectionsCacheDir)
	source := deps.SourceKey(manifest.Root)
	spec := deps.CollectionsSpec{
		CacheRoot: cacheRoot,
		Source:    source,
		Manifest:  manifest,
	}
	if galaxy := filepath.Join(venv.Dir, "bin", galaxyTool); executable(galaxy) {
		spec.GalaxyBinary = galaxy
	}
	// The resolver runs ansible-galaxy, and a requirements file steers it to
	// galaxy servers and installer code no more trusted than a manifest's build
	// backend. Under the sandbox tier it is confined to its collections cache
	// and the requirements, denied the data dir and the keys; the
	// daemon-context resolve stays only where no sandbox can enforce it. The
	// galaxy that actually executes is granted — the venv's sibling when one
	// was resolved, the image's otherwise (ADR 0015).
	if sandboxed(ctx) {
		galaxyCache := filepath.Join(dataDir, galaxyCacheDir)
		// The requirements file may name a local collection artifact
		// (type: dir/file) — typically a sibling checkout outside the project
		// root — that ansible-galaxy must read while resolving. Grant exactly
		// those paths read-only on top of the project root; the profile guard
		// still refuses any that would reach the data dir.
		read := append([]string{manifest.Root}, manifest.LocalSources...)
		if spec.GalaxyBinary != "" {
			read = append(read, executor.ToolGrants(spec.GalaxyBinary)...)
		} else {
			read = append(read, executor.ToolGrants(galaxyTool)...)
		}
		spec.GalaxyCacheDir = galaxyCache
		spec.Sandbox = deps.NewResolverSandbox(deps.ResolverSandboxOptions{
			DataDir:    dataDir,
			VenvDir:    filepath.Join(cacheRoot, source),
			ReadPaths:  read,
			WritePaths: deps.GalaxyCacheGrants(galaxyCache),
		})
	}
	return deps.EnsureCollections(ctx, spec)
}

// executable reports whether path names a runnable file.
func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// ansibleCollectionsPath is the ANSIBLE_COLLECTIONS_PATH value a run gets: the
// per-source resolved collections dir ahead of the shared cache/collections
// fallback when the source resolved collections, the shared dir alone
// otherwise (ADR 0015). The per-source path wins because it comes first.
func ansibleCollectionsPath(dataDir string, collections deps.Collections) string {
	shared := filepath.Join(dataDir, collectionsCacheDir)
	if collections.Dir == "" {
		return shared
	}
	return collections.Dir + string(os.PathListSeparator) + shared
}

// ansiblePlaybookPath returns the playbook file an ansible argv names: its last
// positional argument. ansible-playbook takes playbooks as its only positionals
// and reads options before them, so the tail is the playbook whether or not the
// argv also carries an --inventory.
func ansiblePlaybookPath(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	// An ad-hoc run (`ansible <pattern> ...`) names no playbook: its positionals
	// are a host pattern and module args, not a file whose directory carries a
	// Python manifest. Returning "" keeps ad-hoc runs on the image toolchain and
	// stops a module arg being mistaken for a playbook path.
	if filepath.Base(argv[0]) == executor.AnsibleAdhocBinary {
		return ""
	}
	last := argv[len(argv)-1]
	if strings.HasPrefix(last, "-") {
		return ""
	}
	return last
}

// applyVenv points ansible options at a resolved venv (ADR 0016): its
// site-packages is bridged to the run through PYTHONPATH, its interpreter
// drives the localhost-over-local modules, and its bin leads PATH. Binary is
// set only when the venv's own ansible-playbook exists — a project that
// declared ansible-core shadows the fallback; otherwise Binary stays empty and
// PATH resolves the image fallback toolchain. A zero Venv (no declared deps)
// leaves the options on the image toolchain.
func applyVenv(opts *executor.AnsibleOptions, venv deps.Venv) {
	if venv.Dir == "" {
		return
	}
	if executable(venv.Ansible) {
		opts.Binary = venv.Ansible
	}
	opts.Interpreter = venv.Python
	opts.VenvBin = venv.Bin
	opts.PythonPath = venv.SitePackages
}
