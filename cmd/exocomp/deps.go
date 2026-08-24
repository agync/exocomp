package main

import (
	"context"
	"fmt"
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

// applyVenv points ansible options at a resolved venv: the run executes the
// venv's own ansible-playbook, its interpreter drives the modules, and its bin
// leads PATH. A zero Venv (no declared deps) leaves the options on the image
// toolchain.
func applyVenv(opts *executor.AnsibleOptions, venv deps.Venv) {
	if venv.Dir == "" {
		return
	}
	opts.Binary = venv.Ansible
	opts.Interpreter = venv.Python
	opts.VenvBin = venv.Bin
}
